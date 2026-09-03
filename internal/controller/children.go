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

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
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

	// MetricsService is the headless Service Prometheus scrapes. It is separate
	// from Service on purpose — see naming.MetricsService.
	MetricsService *corev1ac.ServiceApplyConfiguration

	// CanaryDeployment is the variant under evaluation, or nil when no canary
	// should exist. Nil means "delete it if present", which the caller does —
	// an apply cannot express absence.
	CanaryDeployment *appsv1ac.DeploymentApplyConfiguration
}

// renderOptions carries operator-level settings into the pure builders.
//
// They are OPERATOR settings, not spec fields: the shim image is a property of
// the controller's own build, and a user should not have to know it exists to
// get working metrics. Threading it through a struct rather than reading it off
// the reconciler keeps buildChildren a pure function of its arguments, which is
// what makes the whole child-rendering surface table-testable.
type renderOptions struct {
	// ShimImage is the default metrics sidecar image. An empty value falls back
	// to DefaultShimImage, so a zero-valued renderOptions is still usable in a
	// test.
	ShimImage string
}

// shimImage resolves the sidecar image for these options.
func (o renderOptions) shimImage() string {
	if o.ShimImage == "" {
		return DefaultShimImage
	}
	return o.ShimImage
}

// variantPlan is one variant's rendering instructions.
type variantPlan struct {
	// Spec is the SPEC AT THAT REVISION, which during a canary is not the same
	// for both variants: the primary runs the last known-good revision's spec
	// while the canary runs the target's. Reading both from md.Spec would make
	// the two sides of the comparison identical, which is to say it would make
	// the comparison meaningless.
	Spec inferencev1alpha1.ModelDeploymentSpec

	// Revision is that spec's hash.
	Revision string

	// Replicas is this variant's share of spec.replicas.
	Replicas int32
}

// buildChildren renders the full desired state for a ModelDeployment.
//
// It is a pure function: no client, no clock, no I/O. That is what makes it
// exhaustively testable, and it is also what makes Server-Side Apply safe —
// see the byte-stability note on applyConfigFrom.
//
// The canary plan is optional. When nil, only the primary Deployment is
// rendered and the caller is responsible for removing any canary that exists.
func buildChildren(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	primary variantPlan,
	canary *variantPlan,
	opts renderOptions,
) (desiredChildren, error) {
	primaryMD := withSpec(md, primary.Spec)

	dep, err := buildDeployment(primaryMD, prof, primary.Revision,
		inferencev1alpha1.VariantPrimary, primary.Replicas, opts)
	if err != nil {
		return desiredChildren{}, fmt.Errorf("rendering the primary variant: %w", err)
	}

	children := desiredChildren{
		Deployment: dep,
		// Both Services are rendered from the CURRENT spec, never from a
		// revision's. A Service is not versioned: its port and selector are the
		// stable address clients hold, and swapping them mid-rollback would
		// break every caller for the duration of the recovery.
		Service:        buildService(md),
		MetricsService: buildMetricsService(md),
	}

	if canary == nil {
		return children, nil
	}

	canaryMD := withSpec(md, canary.Spec)
	canaryDep, err := buildDeployment(canaryMD, prof, canary.Revision,
		inferencev1alpha1.VariantCanary, canary.Replicas, opts)
	if err != nil {
		return desiredChildren{}, fmt.Errorf("rendering the canary variant: %w", err)
	}
	children.CanaryDeployment = canaryDep

	return children, nil
}

// withSpec returns a shallow copy of md carrying a different spec.
//
// A copy, not a mutation. md comes out of the informer cache and is shared with
// every other reader in the process; writing to it would corrupt what the next
// reconcile — and any other controller in the same manager — sees.
func withSpec(
	md *inferencev1alpha1.ModelDeployment,
	spec inferencev1alpha1.ModelDeploymentSpec,
) *inferencev1alpha1.ModelDeployment {
	out := *md
	out.Spec = spec
	return &out
}

// singleVariant is the plan for a ModelDeployment with no canary in flight.
func singleVariant(md *inferencev1alpha1.ModelDeployment, revision string) variantPlan {
	return variantPlan{Spec: md.Spec, Revision: revision, Replicas: replicasFor(md)}
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
	opts renderOptions,
) (*appsv1ac.DeploymentApplyConfiguration, error) {
	podSpec, err := buildPodSpec(md, prof, variant, opts)
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
//
// The two implemented model sources deliver weights in opposite directions, and
// the difference is confined to this function. An image source PUSHES: an init
// container copies the file onto the shared volume before the engine starts, so
// the engine sees a ready file and needs no network. A huggingFace source PULLS:
// there is no init container at all, and the engine downloads into the same
// volume itself, because llama.cpp already implements Hub downloads and
// reimplementing that in an init container would mean owning retry, resume and
// checksum logic that upstream has already written.
func buildPodSpec(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	variant inferencev1alpha1.Variant,
	opts renderOptions,
) (*corev1ac.PodSpecApplyConfiguration, error) {
	modelPath, cacheDir, initContainers, err := buildModelDelivery(md)
	if err != nil {
		return nil, err
	}

	built, err := prof.Build(engine.BuildContext{
		MD:        md,
		Variant:   variant,
		ModelPath: modelPath,
		CacheDir:  cacheDir,
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

	// The shim is appended AFTER the model init container, and the ordering is
	// meaningful: init containers run in sequence, so the weights are staged
	// first and the sidecar starts second. Putting the sidecar first would work
	// too, but it would leave the shim answering health checks for several
	// minutes while a model image is still being copied — reporting a variant
	// as merely "not ready yet" when it has not begun to exist.
	if md.Spec.Serving.ShimEnabled() {
		shimAC, convErr := applyConfigFrom[corev1ac.ContainerApplyConfiguration](
			buildShimContainer(md, variant, built.HealthPath, opts.shimImage()))
		if convErr != nil {
			return nil, fmt.Errorf("converting shim container: %w", convErr)
		}
		initContainers = append(initContainers, shimAC)
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
		WithInitContainers(initContainers...).
		WithContainers(engineAC).
		WithVolumes(corev1ac.Volume().
			WithName(engine.ModelVolumeName).
			WithEmptyDir(corev1ac.EmptyDirVolumeSource())), nil
}

// buildModelDelivery resolves how weights reach the pod for the configured
// model source.
//
// It returns at most one of modelPath and cacheDir. modelPath names a file that
// will already exist when the engine starts; cacheDir names a writable
// directory the engine is expected to download into. Which one is set is what
// tells the engine profile whether to mount the model volume read-only, so
// returning both — or neither, for an implemented source — would be a bug.
func buildModelDelivery(
	md *inferencev1alpha1.ModelDeployment,
) (modelPath, cacheDir string, initContainers []*corev1ac.ContainerApplyConfiguration, err error) {
	src := md.Spec.Model.Source

	switch {
	case src.Image != nil:
		initAC, convErr := applyConfigFrom[corev1ac.ContainerApplyConfiguration](
			buildModelInitContainer(src.Image))
		if convErr != nil {
			return "", "", nil, fmt.Errorf("converting model init container: %w", convErr)
		}
		return runtimeModelPath(src.Image), "", []*corev1ac.ContainerApplyConfiguration{initAC}, nil

	case src.HuggingFace != nil:
		// No init container: the engine is the downloader. The volume is still
		// an emptyDir, so weights are re-fetched on every pod start — that cost
		// is the documented trade-off of this source, and is why the image
		// source is the recommended one.
		return "", modelMountPath, nil, nil

	default:
		// CEL enforces exactly-one-of on the union and the engine profile
		// rejects sources it cannot serve, so reaching here means a source was
		// added to the API without being wired up. Failing loudly beats
		// emitting a pod with no weights, which would fail its startup probe
		// several minutes later with nothing pointing at the cause.
		return "", "", nil, fmt.Errorf(
			"no implemented model source in .spec.model.source: " +
				"set .image or .huggingFace")
	}
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
			// canary.
			WithSelector(naming.ServiceSelector(md.Name)).
			WithPorts(corev1ac.ServicePort().
				WithName(naming.PortNameHTTP).
				WithPort(port).
				WithProtocol(corev1.ProtocolTCP).
				// Targeted BY NAME, not by number. With the shim injected,
				// "http" resolves to the sidecar; without it, to the engine.
				// That single indirection is the entire mechanism by which
				// turning metrics on or off changes no client, no Service port
				// and no endpoint URL.
				WithTargetPort(intstr.FromString(servingTargetPort(md)))))
}

// servingTargetPort is the container port name the serving Service resolves to.
func servingTargetPort(md *inferencev1alpha1.ModelDeployment) string {
	if md.Spec.Serving.ShimEnabled() {
		return naming.PortNameHTTP
	}
	return naming.PortNameEngine
}

// buildMetricsService renders the headless Service Prometheus scrapes.
//
// # Why this is a second Service
//
// Two reasons, and the second is the one that matters.
//
// The engine's own /metrics has to be reachable for the diagnostics scrape, and
// adding the engine's port to the SERVING Service would publish it to every
// in-cluster client. Any of them could then address the engine directly and
// bypass the shim — at which point that traffic contributes to no llmcp_* series
// at all, and the canary analysis is quietly reasoning about a subset of the
// load while believing it sees all of it. Keeping the engine port off the
// serving Service makes the bypass unavailable rather than merely discouraged.
//
// It is HEADLESS (clusterIP: None) because a scraper wants one target per pod.
// A normal ClusterIP would give prometheus-operator a single VIP that
// load-balances to a different pod on every scrape, producing one series whose
// value jumps between pods — which for a per-pod gauge like queue depth is not
// merely imprecise, it is meaningless.
func buildMetricsService(md *inferencev1alpha1.ModelDeployment) *corev1ac.ServiceApplyConfiguration {
	labels := naming.CommonLabels(md.Name)
	// Overridden so the ServiceMonitor's selector can pick this Service out
	// from the serving one. Both carry the same model-deployment label; the
	// component is what distinguishes them.
	labels[naming.LabelComponent] = observability.ComponentMetrics

	ports := []*corev1ac.ServicePortApplyConfiguration{}

	if md.Spec.Serving.ShimEnabled() {
		ports = append(ports, corev1ac.ServicePort().
			WithName(naming.PortNameMetrics).
			WithPort(naming.ShimMetricsPort).
			WithProtocol(corev1.ProtocolTCP).
			WithTargetPort(intstr.FromString(naming.PortNameMetrics)))
	}

	ports = append(ports, corev1ac.ServicePort().
		WithName(naming.PortNameEngine).
		WithPort(naming.EnginePort).
		WithProtocol(corev1.ProtocolTCP).
		WithTargetPort(intstr.FromString(naming.PortNameEngine)))

	return corev1ac.Service(naming.MetricsService(md.Name), md.Namespace).
		WithLabels(labels).
		WithOwnerReferences(ownerReference(md)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithClusterIP(corev1.ClusterIPNone).
			// PublishNotReadyAddresses so that a pod which is still loading its
			// model is still scraped. Without it the first minutes of every
			// rollout are a hole in the data — precisely the window in which
			// someone is watching to see whether the rollout is working.
			WithPublishNotReadyAddresses(true).
			WithSelector(naming.ServiceSelector(md.Name)).
			WithPorts(ports...))
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
		WithKind(kindModelDeployment).
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
