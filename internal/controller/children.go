/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"path"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
	"github.com/surajmishra/llmcp/internal/engine"
	"github.com/surajmishra/llmcp/internal/naming"
)

const (
	// modelMountPath is where the model volume is mounted in the engine
	// container. It is fixed rather than user-controlled so that the path the
	// engine is pointed at is always predictable, regardless of where the file
	// happened to live inside the model image.
	modelMountPath = "/models"

	// modelStagingPath is where the SAME volume is mounted in the init
	// container that populates it.
	//
	// It must differ from modelMountPath. A volume mount shadows whatever the
	// image already has at that path, so mounting the (empty) model volume over
	// the model image's own directory would hide the very files being copied —
	// and with the CRD's default source path of /models/model.gguf the copy
	// would additionally degenerate into copying a file onto itself. Staging
	// elsewhere keeps the image's own filesystem visible to `cp`.
	modelStagingPath = "/mnt/model"

	// modelInitContainerName is the init container that copies model weights
	// out of the model image and into the shared volume.
	modelInitContainerName = "model-init"

	// defaultRevisionHistoryLimit bounds how many ReplicaSets the child
	// Deployment retains. Our own rollback history lives in ControllerRevisions,
	// so the Deployment's ReplicaSet history is only useful for debugging.
	defaultRevisionHistoryLimit int32 = 3

	// probePeriodSeconds is the interval for readiness and startup probes.
	probePeriodSeconds int32 = 5

	// nonRootUID / nonRootGID are the identity every serving pod runs as.
	//
	// 65532 is the conventional "nonroot" user in distroless images, so the
	// engine container's own image already agrees with it. Fixing the value
	// here rather than inheriting it from each image is what lets an arbitrary
	// model image participate without declaring a USER of its own.
	nonRootUID int64 = 65532
	nonRootGID int64 = 65532
)

// runtimeModelPath is the absolute in-container path the engine will read the
// model from. The basename is preserved from the source so that engines which
// sniff a file extension still behave, and so the path is recognisable in logs.
func runtimeModelPath(src *inferencev1alpha1.ImageModelSource) string {
	return path.Join(modelMountPath, path.Base(src.Path))
}

// desiredChildren is everything the controller applies for one ModelDeployment.
type desiredChildren struct {
	Deployment *appsv1ac.DeploymentApplyConfiguration
	Service    *corev1ac.ServiceApplyConfiguration
}

// buildChildren renders the full desired state for a ModelDeployment at a given
// revision.
//
// It is a pure function: no client, no clock, no I/O. That is what makes it
// exhaustively testable, and it is also what makes Server-Side Apply safe —
// see the byte-stability note on applyConfigFrom.
func buildChildren(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	revision string,
) (desiredChildren, error) {
	dep, err := buildDeployment(md, prof, revision, inferencev1alpha1.VariantPrimary, replicasFor(md))
	if err != nil {
		return desiredChildren{}, err
	}
	return desiredChildren{
		Deployment: dep,
		Service:    buildService(md),
	}, nil
}

// replicasFor returns the desired replica count, defaulting to 1 when unset.
//
// spec.replicas is the TOTAL across all variants. Today there is only a primary
// variant, so it maps straight through; once canaries exist this total is split
// between them rather than added to.
func replicasFor(md *inferencev1alpha1.ModelDeployment) int32 {
	if md.Spec.Replicas == nil {
		return 1
	}
	return *md.Spec.Replicas
}

// buildDeployment renders the Deployment for one variant.
func buildDeployment(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	revision string,
	variant inferencev1alpha1.Variant,
	replicas int32,
) (*appsv1ac.DeploymentApplyConfiguration, error) {
	podSpec, err := buildPodSpec(md, prof, variant)
	if err != nil {
		return nil, err
	}

	name := naming.DeploymentFor(md.Name, variant)

	labels := naming.CommonLabels(md.Name)
	labels[naming.LabelVariant] = string(variant)
	labels[naming.LabelRevision] = revision

	dep := appsv1ac.Deployment(name, md.Namespace).
		WithLabels(labels).
		WithOwnerReferences(ownerReference(md)).
		WithSpec(appsv1ac.DeploymentSpec().
			// The controller always owns the child Deployment's replica count.
			// An autoscaler scales the ModelDeployment through its /scale
			// subresource, never the Deployment directly, so there is exactly
			// one writer here and no field-ownership conflict to arbitrate.
			WithReplicas(replicas).
			WithRevisionHistoryLimit(defaultRevisionHistoryLimit).
			WithProgressDeadlineSeconds(progressDeadlineSeconds(md)).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(naming.SelectorLabels(md.Name, variant))).
			WithStrategy(buildStrategy(md)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(naming.PodLabels(md.Name, variant, revision)).
				WithSpec(podSpec)))

	return dep, nil
}

// buildStrategy maps spec.rollout onto the child Deployment's update strategy.
//
// The mechanics of a rolling update are deliberately delegated to the Deployment
// controller, which already implements them correctly. What this operator adds
// is the part a Deployment lacks: a stalled Deployment stays stalled forever,
// whereas exceeding the progress deadline here is actionable.
func buildStrategy(md *inferencev1alpha1.ModelDeployment) *appsv1ac.DeploymentStrategyApplyConfiguration {
	if md.Spec.Rollout.Type == inferencev1alpha1.RolloutRecreate {
		return appsv1ac.DeploymentStrategy().WithType(appsv1.RecreateDeploymentStrategyType)
	}

	rolling := appsv1ac.RollingUpdateDeployment()
	if md.Spec.Rollout.MaxSurge != nil {
		rolling = rolling.WithMaxSurge(*md.Spec.Rollout.MaxSurge)
	}
	if md.Spec.Rollout.MaxUnavailable != nil {
		rolling = rolling.WithMaxUnavailable(*md.Spec.Rollout.MaxUnavailable)
	}

	return appsv1ac.DeploymentStrategy().
		WithType(appsv1.RollingUpdateDeploymentStrategyType).
		WithRollingUpdate(rolling)
}

// progressDeadlineSeconds converts spec.rollout.progressDeadline to the
// Deployment's field, falling back to the API default when unset.
func progressDeadlineSeconds(md *inferencev1alpha1.ModelDeployment) int32 {
	const fallback int32 = 600
	if md.Spec.Rollout.ProgressDeadline == nil {
		return fallback
	}
	secs := int32(md.Spec.Rollout.ProgressDeadline.Seconds())
	if secs < 1 {
		return fallback
	}
	return secs
}

// buildPodSpec assembles the serving pod: model delivery, then the engine.
func buildPodSpec(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	variant inferencev1alpha1.Variant,
) (*corev1ac.PodSpecApplyConfiguration, error) {
	src := md.Spec.Model.Source.Image
	if src == nil {
		// Validate() should have caught this; failing loudly here rather than
		// producing a pod that cannot possibly start.
		return nil, fmt.Errorf("model source image is required")
	}

	modelPath := runtimeModelPath(src)

	built, err := prof.Build(engine.BuildContext{
		MD:        md,
		Variant:   variant,
		ModelPath: modelPath,
		Threads:   engine.ThreadsFor(&md.Spec, 1),
		Port:      naming.EnginePort,
	})
	if err != nil {
		return nil, fmt.Errorf("building engine container: %w", err)
	}

	engineContainer := built.Container
	withProbes(&engineContainer, built.HealthPath, md)

	engineAC, err := applyConfigFrom[corev1ac.ContainerApplyConfiguration](engineContainer)
	if err != nil {
		return nil, fmt.Errorf("converting engine container: %w", err)
	}

	initAC, err := applyConfigFrom[corev1ac.ContainerApplyConfiguration](
		buildModelInitContainer(src))
	if err != nil {
		return nil, fmt.Errorf("converting model init container: %w", err)
	}

	return corev1ac.PodSpec().
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithRunAsNonRoot(true).
			// RunAsUser/RunAsGroup must be set explicitly, not left to the
			// image. `runAsNonRoot: true` on its own makes the kubelet inspect
			// the image's USER directive and REFUSE to start the pod if it is
			// root — which rejects every image that does not happen to declare
			// a non-root user, including the minimal busybox bases that model
			// images are built on. Pinning the UID here means an arbitrary
			// model image works without its author having to know about this.
			WithRunAsUser(nonRootUID).
			WithRunAsGroup(nonRootGID).
			// FSGroup makes the shared model volume group-writable. Without it
			// the emptyDir is owned by root and the non-root init container
			// cannot write the weights into it.
			WithFSGroup(nonRootGID).
			WithSeccompProfile(corev1ac.SeccompProfile().
				WithType(corev1.SeccompProfileTypeRuntimeDefault))).
		WithInitContainers(initAC).
		WithContainers(engineAC).
		WithVolumes(corev1ac.Volume().
			WithName(engine.ModelVolumeName).
			WithEmptyDir(corev1ac.EmptyDirVolumeSource())), nil
}

// buildModelInitContainer copies model weights out of the model image into the
// shared volume before the engine starts.
//
// The model ships as its own OCI image rather than being baked into the engine
// image, so that a model change and an engine change are independently
// deployable — which is what will later allow either to be canaried on its own.
//
// This requires the model image to provide a `cp` binary, so model images are
// built on a minimal shell base (busybox) rather than `scratch`.
func buildModelInitContainer(src *inferencev1alpha1.ImageModelSource) corev1.Container {
	// Copy into the staging mount, under the same basename the engine will
	// later read from modelMountPath.
	dest := path.Join(modelStagingPath, path.Base(src.Path))

	return corev1.Container{
		Name:            modelInitContainerName,
		Image:           src.Image,
		ImagePullPolicy: src.PullPolicy,
		Command:         []string{"cp", src.Path, dest},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      engine.ModelVolumeName,
			MountPath: modelStagingPath,
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// withProbes attaches health probes to the engine container.
//
// The three probes have distinct jobs and the startup probe is the load-bearing
// one: model load dominates startup and can take orders of magnitude longer
// than steady-state health checks. A liveness probe alone would kill the
// container mid-load and produce a crash loop that looks like a broken image.
// The startup probe suppresses both other probes until the engine reports ready
// once, which is exactly what spec.serving.startupTimeout budgets for.
//
// llama.cpp's /health is genuinely correct here — it returns 503 while loading
// and 200 only once the model is resident — so readiness needs no extra logic.
func withProbes(c *corev1.Container, healthPath string, md *inferencev1alpha1.ModelDeployment) {
	probe := func() *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt32(naming.EnginePort),
				},
			},
			PeriodSeconds: probePeriodSeconds,
		}
	}

	startup := probe()
	startup.FailureThreshold = startupFailureThreshold(md)
	c.StartupProbe = startup

	readiness := probe()
	readiness.FailureThreshold = 3
	readiness.SuccessThreshold = 1
	readiness.TimeoutSeconds = 3
	c.ReadinessProbe = readiness

	liveness := probe()
	// Deliberately slack: the startup probe has already proven the engine can
	// serve, so liveness only needs to catch a genuinely wedged process. A
	// tight threshold here would restart pods that are merely slow under load.
	liveness.FailureThreshold = 6
	liveness.TimeoutSeconds = 5
	liveness.PeriodSeconds = 10
	c.LivenessProbe = liveness
}

// startupFailureThreshold converts spec.serving.startupTimeout into probe
// attempts, rounding up so the configured budget is never under-honoured.
func startupFailureThreshold(md *inferencev1alpha1.ModelDeployment) int32 {
	const fallbackSeconds = 300
	seconds := int32(fallbackSeconds)
	if md.Spec.Serving.StartupTimeout != nil {
		if s := int32(md.Spec.Serving.StartupTimeout.Seconds()); s > 0 {
			seconds = s
		}
	}
	threshold := max((seconds+probePeriodSeconds-1)/probePeriodSeconds, 1)
	return threshold
}

// buildService renders the Service that fronts every variant.
func buildService(md *inferencev1alpha1.ModelDeployment) *corev1ac.ServiceApplyConfiguration {
	port := md.Spec.Serving.Port
	if port == 0 {
		port = naming.ServicePort
	}

	return corev1ac.Service(naming.Service(md.Name), md.Namespace).
		WithLabels(naming.CommonLabels(md.Name)).
		WithOwnerReferences(ownerReference(md)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			// Spans every variant, so one endpoint serves both sides of a
			// future canary.
			WithSelector(naming.ServiceSelector(md.Name)).
			WithPorts(corev1ac.ServicePort().
				WithName(naming.PortNameHTTP).
				WithPort(port).
				WithProtocol(corev1.ProtocolTCP).
				// Targeted BY NAME, not by number. When the metrics shim is
				// introduced it takes over the "http" container port and this
				// Service needs no change at all.
				WithTargetPort(intstr.FromString(naming.PortNameHTTP))))
}

// ownerReference builds the controller owner reference for child objects.
//
// Every child is owned, so Kubernetes garbage collection deletes them when the
// ModelDeployment goes away. That is why this operator has no finalizer: a
// finalizer that is not strictly required is a liability, since a bug in its
// removal path wedges deletion permanently.
func ownerReference(md *inferencev1alpha1.ModelDeployment) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(inferencev1alpha1.GroupVersion.String()).
		WithKind("ModelDeployment").
		WithName(md.Name).
		WithUID(md.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

// applyConfigFrom converts a typed API object into its apply configuration.
//
// Apply configurations mirror their API types field for field and share JSON
// tags, so a marshal/unmarshal round trip is a faithful, if unglamorous,
// conversion — and it lets engine profiles keep returning ordinary
// corev1.Container values, which are far easier to construct and to assert on
// in tests.
//
// The `omitempty`-less fields matter here: a typed struct marshals an empty
// ResourceRequirements as `"resources":{}`, which under Server-Side Apply
// claims ownership of a field the caller never set. Callers that care strip
// such fields before converting.
func applyConfigFrom[T any](obj any) (*T, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshalling %T: %w", obj, err)
	}
	out := new(T)
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("unmarshalling into %T: %w", out, err)
	}
	return out, nil
}

// serviceSelectorString renders the /scale subresource's labelSelector.
//
// This must be a SERIALIZED selector string, not a metav1.LabelSelector.
// Getting that wrong is the single most common defect in CRD scale
// implementations: the CRD still validates, `kubectl scale` still works, and
// only an HPA reveals the bug — by silently failing to find any pods and
// therefore computing its target from nothing.
func serviceSelectorString(md *inferencev1alpha1.ModelDeployment) (string, error) {
	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: naming.ServiceSelector(md.Name),
	})
	if err != nil {
		return "", fmt.Errorf("serialising status selector: %w", err)
	}
	return sel.String(), nil
}
