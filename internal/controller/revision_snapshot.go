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
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/revision"
)

const (
	defaultModelImagePath = "/weights/model.gguf"
	defaultShimLogLevel   = "info"
	defaultStartupTimeout = 300 * time.Second
	defaultContextSize    = int32(4096)
	defaultMaxConcurrency = int32(4)
)

// resolvedRevisionSpec freezes every API- or operator-level value that affects
// a serving Pod. The versioned ControllerRevision stores this projection, so
// rendering historical workloads does not consult defaults compiled into a
// newer controller.
func resolvedRevisionSpec(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	opts renderOptions,
) inferencev1alpha1.ModelDeploymentSpec {
	resolved := *md.Spec.DeepCopy()

	if image := resolved.Model.Source.Image; image != nil {
		if image.Path == "" {
			image.Path = defaultModelImagePath
		}
		if image.PullPolicy == "" {
			image.PullPolicy = corev1.PullIfNotPresent
		}
	}

	if resolved.Engine.Type == "" {
		resolved.Engine.Type = prof.Type()
	}
	if resolved.Engine.Image == "" {
		resolved.Engine.Image = prof.DefaultImage()
	}
	if resolved.Engine.ImagePullPolicy == "" {
		resolved.Engine.ImagePullPolicy = corev1.PullIfNotPresent
	}
	if resolved.Engine.ContextSize == nil {
		resolved.Engine.ContextSize = ptr.To(defaultContextSize)
	}
	if resolved.Engine.MaxConcurrency == nil {
		resolved.Engine.MaxConcurrency = ptr.To(defaultMaxConcurrency)
	}
	if resolved.Engine.Threads == nil {
		resolved.Engine.Threads = ptr.To(engine.ThreadsFor(&resolved, 1))
	}

	if resolved.Serving.StartupTimeout == nil {
		resolved.Serving.StartupTimeout = &metav1.Duration{Duration: defaultStartupTimeout}
	}

	enabled := resolved.Serving.ShimEnabled()
	if !enabled {
		resolved.Serving.Shim = inferencev1alpha1.ShimSpec{Enabled: ptr.To(false)}
		return resolved
	}
	resolved.Serving.Shim.Enabled = ptr.To(true)
	if resolved.Serving.Shim.Image == "" {
		resolved.Serving.Shim.Image = opts.shimImage()
	}
	if resolved.Serving.Shim.LogLevel == "" {
		resolved.Serving.Shim.LogLevel = defaultShimLogLevel
	}
	resolved.Serving.Shim.Resources = shimResources(resolved.Serving.Shim)
	return resolved
}

// mergeWorkloadSpec overlays a stored workload on the live policy. Replicas,
// rollout, autoscaling, observability, and the current Service port remain live
// for v2 because none defines a serving Pod. Legacy payloads historically
// treated the Service port as revision data, so their decoder preserves it
// until the one-time migration has completed.
func mergeWorkloadSpec(
	live inferencev1alpha1.ModelDeploymentSpec,
	workload inferencev1alpha1.ModelDeploymentSpec,
	legacy bool,
) inferencev1alpha1.ModelDeploymentSpec {
	merged := *live.DeepCopy()
	merged.Model = *workload.Model.DeepCopy()
	merged.Engine = *workload.Engine.DeepCopy()
	merged.Serving.StartupTimeout = workload.Serving.StartupTimeout.DeepCopy()
	merged.Serving.Shim = *workload.Serving.Shim.DeepCopy()
	if legacy {
		merged.Serving.Port = workload.Serving.Port
	}
	return merged
}

// legacyMigrationReference identifies the persisted target whose hash-format
// transition could otherwise be mistaken for a new workload. deployment is
// the live evidence for that target. noRender is reserved for the two terminal
// canary states in which the rejected target is intentionally not rendered.
type legacyMigrationReference struct {
	revision   string
	deployment *appsv1.Deployment
	variant    inferencev1alpha1.Variant
	noRender   bool
}

// targetRevisionForMigration chooses the identity the rollout state machine
// sees during the one-time legacy-to-v2 transition.
//
// The default answer is always the current v2 identity. A legacy identity is
// adopted only when all evidence lines up:
//   - status and the live Deployment name the same stored ControllerRevision;
//   - the stored bytes are the canonical bytes for that legacy identity;
//   - every workload field recorded by the legacy schema still matches; and
//   - the current resolved workload renders as the live primary/canary Pod.
//
// Missing or contradictory evidence is an error, not a best-effort rollout.
// That preserves the serving children and persisted canary counters/timestamps
// until an operator restores the history/evidence needed to make a safe choice.
func (r *ModelDeploymentReconciler) targetRevisionForMigration(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	resolved inferencev1alpha1.ModelDeploymentSpec,
	v2Revision string,
	primary, canary *appsv1.Deployment,
) (string, bool, bool, error) {
	ref, ok := legacyReferenceFrom(md, primary, canary)
	if !ok || ref.revision == "" || ref.revision == v2Revision {
		return v2Revision, false, false, nil
	}

	if r.revisions == nil {
		r.revisions = &revision.Recorder{Client: r.Client, Scheme: r.Scheme}
	}
	cr, err := r.revisions.Get(ctx, md, ref.revision)
	if apierrors.IsNotFound(err) {
		return "", false, false, fmt.Errorf(
			"persisted rollout identity %s has no ControllerRevision; refusing to infer whether the v2 hash is only a schema change: %w",
			ref.revision, err)
	}
	if err != nil {
		return "", false, false, fmt.Errorf("reading persisted rollout identity %s: %w", ref.revision, err)
	}

	stored, err := revision.Decode(cr.Data.Raw)
	if err != nil {
		return "", false, false, fmt.Errorf("decoding persisted rollout identity %s: %w", ref.revision, err)
	}
	if !stored.Legacy() {
		// The prior target already uses a versioned identity. A different v2
		// hash is therefore a real workload change, not a schema migration.
		return v2Revision, false, false, nil
	}

	canonical, err := revision.Encode(&stored.Spec)
	if err != nil {
		return "", false, false, fmt.Errorf("re-encoding legacy rollout identity %s: %w", ref.revision, err)
	}
	if !bytes.Equal(canonical, cr.Data.Raw) || revision.Hash(&stored.Spec) != ref.revision {
		return "", false, false, fmt.Errorf(
			"ControllerRevision %s does not contain the canonical payload for its persisted identity; refusing legacy adoption",
			ref.revision)
	}

	if !legacyRecordedWorkloadEqual(md.Spec, stored.Spec) {
		// A user-authored workload field changed. It is a real candidate and
		// must use the v2 identity so all effective values are frozen.
		return v2Revision, false, false, nil
	}

	if ref.noRender {
		// RollingBack and sticky-failed states deliberately do not render the
		// rejected target, so there may no longer be a candidate Deployment to
		// inspect. The legacy bytes prove unchanged recorded inputs. The only
		// field the old schema omitted outright was startupTimeout, which is
		// shared by stable and candidate, so the live stable Pod can prove it.
		if !startupTimeoutMatchesDeployment(md, resolved, primary) {
			return "", false, false, fmt.Errorf(
				"legacy rejected revision %s has no live candidate and the primary Deployment does not prove its omitted serving.startupTimeout; refusing to guess equivalence",
				ref.revision)
		}
		return ref.revision, true, false, nil
	}

	if ref.deployment == nil {
		return "", false, false, fmt.Errorf(
			"legacy rollout identity %s has no matching live %s Deployment; refusing to guess effective runtime defaults",
			ref.revision, ref.variant)
	}
	if got := ref.deployment.Spec.Template.Labels[naming.LabelRevision]; got != ref.revision {
		return "", false, false, fmt.Errorf(
			"legacy rollout identity %s conflicts with live %s Deployment revision %q",
			ref.revision, ref.variant, got)
	}

	match, err := currentWorkloadMatchesDeployment(md, prof, resolved, ref, r.renderOptions())
	if err != nil {
		return "", false, false, fmt.Errorf("comparing legacy rollout identity %s with its live workload: %w", ref.revision, err)
	}
	if match == workloadDifferent {
		// The live Pod is authoritative evidence that effective runtime
		// configuration changed (for example an explicitly changed startup
		// timeout or a different operator-provided image). Use a controlled v2
		// candidate rather than relabeling that change as the old revision.
		return v2Revision, false, false, nil
	}

	// A steady deployment may refresh its telemetry producer without turning
	// that controller-owned compatibility change into a user release. During
	// an active rollout, however, changing either variant invalidates the rung's
	// readiness window and metric evidence. Hold both Deployments untouched;
	// the abort annotation is the explicit, safe escape hatch handled by
	// Reconcile before any apply.
	hold := match == workloadLegacyTelemetry && legacyRolloutActive(md)
	return ref.revision, true, hold, nil
}

type workloadMigrationMatch uint8

const (
	workloadDifferent workloadMigrationMatch = iota
	workloadExact
	workloadLegacyTelemetry
)

func legacyRolloutActive(md *inferencev1alpha1.ModelDeployment) bool {
	switch md.Status.Phase {
	case inferencev1alpha1.PhaseCanarying, inferencev1alpha1.PhasePaused,
		inferencev1alpha1.PhasePromoting:
		return true
	default:
		return false
	}
}

// startupTimeoutMatchesDeployment checks the one pod-affecting field omitted
// wholesale by the legacy payload. Startup timeout is release-independent, so
// the stable primary is valid evidence after a rejected candidate was deleted.
func startupTimeoutMatchesDeployment(
	md *inferencev1alpha1.ModelDeployment,
	resolved inferencev1alpha1.ModelDeploymentSpec,
	primary *appsv1.Deployment,
) bool {
	if primary == nil {
		return false
	}
	stable := firstNonEmpty(md.Status.LastGoodRevision, md.Status.StableRevision)
	if stable == "" || primary.Spec.Template.Labels[naming.LabelRevision] != stable {
		return false
	}
	c, ok := containerNamed(primary.Spec.Template.Spec.Containers, engine.ContainerName)
	if !ok || c.StartupProbe == nil {
		return false
	}
	target := withSpec(md, resolved)
	return c.StartupProbe.PeriodSeconds == probePeriodSeconds &&
		c.StartupProbe.FailureThreshold == startupFailureThreshold(target)
}

// legacyReferenceFrom maps persisted rollout state to the Deployment that can
// prove its effective configuration. Status wins over inference from workload
// names: its identity and phase are the restart contract.
func legacyReferenceFrom(
	md *inferencev1alpha1.ModelDeployment,
	primary, canary *appsv1.Deployment,
) (legacyMigrationReference, bool) {
	if md == nil {
		return legacyMigrationReference{}, false
	}
	cs := md.Status.Canary
	if cs != nil && cs.Revision != "" {
		switch md.Status.Phase {
		case inferencev1alpha1.PhaseCanarying, inferencev1alpha1.PhasePaused:
			return legacyMigrationReference{revision: cs.Revision, deployment: canary,
				variant: inferencev1alpha1.VariantCanary}, true
		case inferencev1alpha1.PhasePromoting:
			return legacyMigrationReference{revision: cs.Revision, deployment: primary,
				variant: inferencev1alpha1.VariantPrimary}, true
		case inferencev1alpha1.PhaseRollingBack:
			return legacyMigrationReference{revision: cs.Revision,
				variant: inferencev1alpha1.VariantCanary, noRender: true}, true
		}
	}
	if cs != nil && cs.FailedRevision != "" {
		return legacyMigrationReference{revision: cs.FailedRevision,
			variant: inferencev1alpha1.VariantCanary, noRender: true}, true
	}

	if primary == nil {
		return legacyMigrationReference{}, false
	}
	rev := primary.Spec.Template.Labels[naming.LabelRevision]
	if rev == "" || (rev != md.Status.LastGoodRevision && rev != md.Status.StableRevision) {
		return legacyMigrationReference{}, false
	}
	return legacyMigrationReference{revision: rev, deployment: primary,
		variant: inferencev1alpha1.VariantPrimary}, true
}

// legacyRecordedWorkloadEqual compares exactly the pod-affecting fields the
// old payload knew how to store. Service port is intentionally ignored: v2
// correctly moved it outside workload identity. startupTimeout is handled by
// rendered-workload evidence because legacy never stored it.
func legacyRecordedWorkloadEqual(
	live, stored inferencev1alpha1.ModelDeploymentSpec,
) bool {
	return apiequality.Semantic.DeepEqual(live.Model, stored.Model) &&
		apiequality.Semantic.DeepEqual(live.Engine, stored.Engine) &&
		apiequality.Semantic.DeepEqual(live.Serving.Shim, stored.Serving.Shim)
}

// currentWorkloadMatchesDeployment renders today's fully resolved target under
// the persisted identity and checks that every declared Pod-template field is
// already present in the live object. DeepDerivative intentionally tolerates
// API-server defaults and unrelated metadata added by other controllers.
func currentWorkloadMatchesDeployment(
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	resolved inferencev1alpha1.ModelDeploymentSpec,
	ref legacyMigrationReference,
	opts renderOptions,
) (workloadMigrationMatch, error) {
	desired, err := buildDeployment(withSpec(md, resolved), prof, ref.revision,
		ref.variant, replicasFor(md), opts)
	if err != nil {
		return workloadDifferent, err
	}
	typed, err := applyConfigFrom[appsv1.Deployment](desired)
	if err != nil {
		return workloadDifferent, err
	}
	applyPodStorageDefaults(&typed.Spec.Template.Spec)
	if apiequality.Semantic.DeepDerivative(typed.Spec.Template, ref.deployment.Spec.Template) {
		return workloadExact, nil
	}

	// PR-02 added these two arguments to identify the namespace-scoped metric
	// producer. Their absence is a known, narrow pre-migration shape rather
	// than evidence that the model or engine workload changed. Strip them only
	// from the desired comparison; every other argument, image, resource,
	// probe, volume and security field must still match the live Pod template.
	legacyShape := typed.DeepCopy()
	if shim, ok := containerNamed(legacyShape.Spec.Template.Spec.InitContainers, naming.ShimContainerName); ok {
		shim.Args = withoutMetricScopeArgs(shim.Args)
		for i := range legacyShape.Spec.Template.Spec.InitContainers {
			if legacyShape.Spec.Template.Spec.InitContainers[i].Name == naming.ShimContainerName {
				legacyShape.Spec.Template.Spec.InitContainers[i] = shim
				break
			}
		}
	}
	if apiequality.Semantic.DeepDerivative(legacyShape.Spec.Template, ref.deployment.Spec.Template) {
		return workloadLegacyTelemetry, nil
	}
	return workloadDifferent, nil
}

// applyPodStorageDefaults mirrors the stable core/v1 defaults materialized by
// the API server in a stored Pod template. DeepDerivative ignores empty strings
// and nil pointers but not numeric zero, so a desired probe timeout of zero
// (meaning default) otherwise differs from the stored value one. Keeping this
// normalization local and explicit also makes a future Kubernetes default
// change fail closed until it is reviewed here.
func applyPodStorageDefaults(spec *corev1.PodSpec) {
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = corev1.RestartPolicyAlways
	}
	if spec.TerminationGracePeriodSeconds == nil {
		spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
	}
	if spec.DNSPolicy == "" {
		spec.DNSPolicy = corev1.DNSClusterFirst
	}
	if spec.SchedulerName == "" {
		spec.SchedulerName = corev1.DefaultSchedulerName
	}
	defaultContainers := func(containers []corev1.Container) {
		for i := range containers {
			c := &containers[i]
			if c.TerminationMessagePath == "" {
				c.TerminationMessagePath = corev1.TerminationMessagePathDefault
			}
			if c.TerminationMessagePolicy == "" {
				c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
			}
			for _, probe := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
				if probe == nil {
					continue
				}
				if probe.TimeoutSeconds == 0 {
					probe.TimeoutSeconds = 1
				}
				if probe.SuccessThreshold == 0 {
					probe.SuccessThreshold = 1
				}
				if probe.HTTPGet != nil && probe.HTTPGet.Scheme == "" {
					probe.HTTPGet.Scheme = corev1.URISchemeHTTP
				}
			}
		}
	}
	defaultContainers(spec.InitContainers)
	defaultContainers(spec.Containers)
}

func withoutMetricScopeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "--namespace=") || strings.HasPrefix(arg, "--model-deployment=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// resolveLegacyRevisionSpec supplies effective values the original payload did
// not record from the Deployment that is still running under that identity.
// Guessing would let an operator upgrade mutate the stable workload before its
// candidate was judged. If the matching Deployment is already gone, failing
// closed is the only evidence-preserving outcome.
func (r *ModelDeploymentReconciler) resolveLegacyRevisionSpec(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	revisionID string,
	stored inferencev1alpha1.ModelDeploymentSpec,
	prof engine.Profile,
) (inferencev1alpha1.ModelDeploymentSpec, error) {
	primary, err := r.deploymentFor(ctx, md, naming.PrimaryDeployment(md.Name))
	if err != nil {
		return inferencev1alpha1.ModelDeploymentSpec{}, err
	}
	if primary == nil || primary.Spec.Template.Labels[naming.LabelRevision] != revisionID {
		return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf(
			"legacy revision %s omitted effective runtime defaults and no matching primary Deployment remains; "+
				"refusing to guess rollback configuration", revisionID)
	}

	engineContainer, ok := containerNamed(primary.Spec.Template.Spec.Containers, engine.ContainerName)
	if !ok || engineContainer.Image == "" || engineContainer.StartupProbe == nil ||
		engineContainer.StartupProbe.PeriodSeconds < 1 || engineContainer.StartupProbe.FailureThreshold < 1 {
		return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf(
			"legacy revision %s cannot recover its engine image and startup probe from the primary Deployment", revisionID)
	}
	stored.Engine.Image = engineContainer.Image
	stored.Engine.ImagePullPolicy = engineContainer.ImagePullPolicy
	seconds := int64(engineContainer.StartupProbe.PeriodSeconds) *
		int64(engineContainer.StartupProbe.FailureThreshold)
	stored.Serving.StartupTimeout = &metav1.Duration{Duration: time.Duration(seconds) * time.Second}

	if stored.Serving.ShimEnabled() {
		shim, found := containerNamed(primary.Spec.Template.Spec.InitContainers, naming.ShimContainerName)
		if !found || shim.Image == "" {
			return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf(
				"legacy revision %s cannot recover its shim image from the primary Deployment", revisionID)
		}
		stored.Serving.Shim.Image = shim.Image
		stored.Serving.Shim.Resources = *shim.Resources.DeepCopy()
		if stored.Serving.Shim.LogLevel == "" {
			for _, arg := range shim.Args {
				if level, found := strings.CutPrefix(arg, "--log-level="); found {
					stored.Serving.Shim.LogLevel = level
					break
				}
			}
		}
	}

	legacyMD := md.DeepCopy()
	legacyMD.Spec = stored
	return resolvedRevisionSpec(legacyMD, prof, r.renderOptions()), nil
}

func containerNamed(containers []corev1.Container, name string) (corev1.Container, bool) {
	for i := range containers {
		if containers[i].Name == name {
			return containers[i], true
		}
	}
	return corev1.Container{}, false
}
