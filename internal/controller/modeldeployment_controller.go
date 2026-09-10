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
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/discovery"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/engine"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/revision"
)

// revisionHistoryLimit bounds how many ControllerRevisions are retained per
// ModelDeployment. Deep history has no value here — a rollback target older
// than a handful of revisions is almost certainly not what an operator wants —
// and unbounded history is an etcd leak.
const revisionHistoryLimit int32 = 10

// ModelDeploymentReconciler reconciles a ModelDeployment.
//
// The reconcile loop is deliberately thin: fetch, plan, apply, observe, report.
// All the logic worth testing lives in pure functions (buildChildren,
// computeStatus, revision.NewSnapshot, engine.Profile.Build) that take no client and
// no clock, so the bulk of the test suite needs neither an API server nor
// wall-clock waiting.
type ModelDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Clock is injected so that anything time-dependent is deterministic under
	// test. Production passes a real clock; tests pass a fake one and step it.
	Clock clock.PassiveClock

	// Discovery answers whether optional CRDs — prometheus-operator's, today —
	// are served by this cluster.
	//
	// It is an interface and it is allowed to be nil. Nil means "nothing was
	// checked", which is reported as an Unknown MetricsRegistered condition
	// rather than assumed to be an absence: a unit test that constructs a bare
	// reconciler should not thereby assert something about a cluster.
	Discovery discovery.Interface

	// ShimImage is the metrics sidecar image injected into every serving pod.
	// Empty falls back to DefaultShimImage.
	ShimImage string

	// Provider overrides the metric backend used by canary analysis.
	//
	// Production leaves it nil and one is built from
	// spec.rollout.canary.analysis.provider. Tests inject a scripted provider
	// so that a rollout's verdict sequence — "pass, pass, fail, fail" — is a
	// property of the test rather than of whatever Prometheus happened to
	// scrape, which is the only way to assert that rollback fires on exactly
	// the second failure.
	Provider analysis.Provider

	// revisions persists rollout history. Built lazily from Client so that a
	// zero-value reconciler constructed in a test still works.
	revisions *revision.Recorder
}

// renderOptions returns the operator-level settings the pure builders need.
func (r *ModelDeploymentReconciler) renderOptions() renderOptions {
	return renderOptions{ShimImage: r.ShimImage}
}

// now reads the injected clock.
//
// Nothing in this package calls time.Now() directly, and a lint rule forbids
// it outside main. That is what lets an entire five-step canary — including its
// analysis interval and its warm-up delay — be replayed in microseconds by
// stepping a fake clock, instead of a test suite that sleeps for minutes and
// flakes when a CI runner is busy.
func (r *ModelDeploymentReconciler) now() time.Time {
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	return r.Clock.Now()
}

// +kubebuilder:rbac:groups=inference.llmcp.io,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmcp.io,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmcp.io,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmcp.io,resources=modeldeployments/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Events moved to the events.k8s.io API group in controller-runtime v0.23. The
// core-group rule is kept alongside it because the recorder still writes there
// on older clusters — and because a missing events permission fails SILENTLY,
// leaving no audit trail and no error to notice.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one ModelDeployment towards its desired state.
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var md inferencev1alpha1.ModelDeployment
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		if apierrors.IsNotFound(err) {
			// The normal path after a delete: every child carries an owner
			// reference, so garbage collection has already cleaned up the
			// objects. What it cannot clean up is this process's own metric
			// gauges, and a gauge for a resource that no longer exists is worse
			// than a leak — it keeps reporting a canary weight for a rollout
			// that ended, holding dashboards and alerts on a deployment nobody
			// can look at any more.
			//
			// The model label is unknown here (the object is gone), so the
			// sweep matches on namespace and name alone.
			observability.ForgetNamespacedName(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// A resource being deleted needs nothing from us. There is no finalizer by
	// design, so we simply stop touching it.
	if !md.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := validateCanaryRouting(&md.Spec); err != nil {
		return r.failTerminally(ctx, &md, err)
	}
	prof, err := engine.Get(md.Spec.Engine.Type)
	if err != nil {
		return r.failTerminally(ctx, &md, err)
	}
	if err := prof.Validate(&md.Spec); err != nil {
		return r.failTerminally(ctx, &md, err)
	}

	resolvedSpec := resolvedRevisionSpec(&md, prof, r.renderOptions())
	snapshot, err := revision.NewSnapshot(&resolvedSpec)
	if err != nil {
		return r.failTerminally(ctx, &md, err)
	}

	primaryDep, err := r.deploymentFor(ctx, &md, naming.PrimaryDeployment(md.Name))
	if err != nil {
		return ctrl.Result{}, err
	}
	canaryDep, err := r.canaryDeploymentFor(ctx, &md)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The v2 payload deliberately has a different wire identity from legacy
	// history. On an operator upgrade that fact alone must not look like a user
	// changed the workload: doing so would restart an active canary, retry a
	// sticky failed revision, or launch a new steady-state rollout. Reuse the
	// persisted legacy identity only after the migration resolver proves that
	// its stored payload and matching live workload describe today's target.
	rev, adoptedLegacy, holdLegacy, err := r.targetRevisionForMigration(
		ctx, &md, prof, resolvedSpec, snapshot.Revision, primaryDep, canaryDep)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving revision migration: %w", err)
	}
	if holdLegacy && !annotationTrue(&md, naming.AnnoAbort) {
		if err := r.holdLegacyTelemetryMigration(ctx, &md, rev); err != nil {
			return ctrl.Result{}, err
		}
		// This is an intentional operator-action hold, not a transient failure.
		// No timer is needed: changing the abort annotation wakes the watch.
		return ctrl.Result{}, nil
	}
	if !adoptedLegacy {
		if err := r.recordRevision(ctx, &md, rev, snapshot.Raw); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording revision: %w", err)
		}
	}

	// Autoscaling runs BEFORE the rollout is planned, so a scale decision and
	// the canary split that distributes it happen in the same pass. Planned
	// first, the split would divide last pass's total and the fleet would spend
	// a full analysis window at the wrong size.
	//
	// Its error is captured rather than returned, for the same reason the
	// metrics verdict's is: a failed sample must still leave an accurate status
	// behind, and returning here would skip the status write entirely.
	scaling, scaleErr := r.reconcileAutoscaling(ctx, &md, primaryDep, canaryDep)

	// Decide what SHOULD be running before touching anything. The plan is a
	// description of the desired end state, not a list of operations, so the
	// same code path applies a canary at 20%, a promotion, a rollback and a
	// plain rolling update.
	plan := r.planRollout(ctx, &md, rev, primaryDep, canaryDep)

	desired, err := r.renderRollout(ctx, &md, prof, resolvedSpec, rev, plan)
	if err != nil {
		// A build failure is a spec the controller cannot render. Retrying
		// cannot fix it, so do not spin.
		return r.failTerminally(ctx, &md, err)
	}

	if err := r.applyChildren(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}

	if desired.CanaryDeployment == nil {
		if err := r.deleteCanaryDeployment(ctx, &md); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.emitRolloutEvents(&md, plan)

	// Consume promote/abort now that they have been acted on, so an operator's
	// earlier "yes" cannot silently approve the next release too.
	if err := r.clearRolloutAnnotations(ctx, &md); err != nil {
		return ctrl.Result{}, err
	}

	// Metric collection is reconciled before status is computed, so the
	// MetricsRegistered condition reflects this pass rather than the previous
	// one. Its error is captured, not returned immediately: a ServiceMonitor
	// that could not be applied must still leave an accurate status behind, and
	// returning here would skip the status write entirely.
	verdict, metricsErr := r.reconcileObservability(ctx, &md)
	r.noteMetricsUnavailable(&md, verdict)

	obs, err := r.observe(ctx, &md, rev, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	obs.Metrics = verdict
	obs.Scaling = scaling

	if err := r.updateStatus(ctx, &md, obs); err != nil {
		return ctrl.Result{}, err
	}

	// Both deferred errors are reported only now that status is durable, so the
	// retry that follows starts from an accurate picture rather than a stale
	// one. Metrics first: it is the more common failure and the one whose
	// message points at the likelier cause.
	if metricsErr != nil {
		return ctrl.Result{}, metricsErr
	}
	if scaleErr != nil {
		return ctrl.Result{}, scaleErr
	}

	log.V(1).Info("reconciled",
		"revision", rev,
		"primaryRevision", plan.PrimaryRevision,
		"canaryRevision", plan.CanaryRevision,
		"action", plan.Output.Action)

	// Workload watches handle readiness changes. Timers handle rollout analysis,
	// autoscaling, and optional monitoring discovery/drift; the earliest wins.
	return ctrl.Result{RequeueAfter: soonest(soonest(plan.RequeueAfter, scaling.RequeueAfter), verdict.RequeueAfter)}, nil
}

// holdLegacyTelemetryMigration exposes an actionable, durable hold without
// touching either serving Deployment or any canary evidence. Returning a normal
// error here would create an endless exponential-backoff loop for a condition
// only a human can resolve; returning silently would make the paused rollout
// indistinguishable from a stuck controller.
func (r *ModelDeploymentReconciler) holdLegacyTelemetryMigration(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	revisionID string,
) error {
	message := fmt.Sprintf(
		"Legacy rollout %s is using pre-migration telemetry. Serving workloads, rung, counters, and timestamps are preserved; set %s=true to abort safely before telemetry migration",
		revisionID, naming.AnnoAbort)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest inferencev1alpha1.ModelDeployment
		if err := r.Get(ctx, client.ObjectKeyFromObject(md), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		status := *latest.Status.DeepCopy()
		if status.Canary == nil || status.Canary.Revision != revisionID {
			return fmt.Errorf("legacy rollout state changed while publishing the telemetry migration hold")
		}
		status.Canary.MetricScopeReady = false
		status.Canary.Message = message
		setCondition(&status, metav1.Condition{
			Type:               inferencev1alpha1.ConditionMetricScopeReady,
			Status:             metav1.ConditionFalse,
			Reason:             inferencev1alpha1.ReasonMetricsScopePending,
			Message:            message,
			ObservedGeneration: latest.Generation,
		})
		if apiequality.Semantic.DeepEqual(latest.Status, status) {
			return nil
		}
		latest.Status = status
		return r.Status().Update(ctx, &latest)
	})
}

// soonest returns the smallest positive duration, or zero when neither is set.
func soonest(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// renderRollout resolves each variant's spec and renders the child objects.
//
// The per-variant spec lookup is what makes a canary a real comparison. The
// primary must run the LAST KNOWN-GOOD revision's pod template while the canary
// runs the target's, and the known-good template lives in a ControllerRevision
// rather than in the live spec — which by definition describes the new thing.
// Rendering both from md.Spec would produce two identical variants and an
// analysis that compares a revision against itself.
func (r *ModelDeploymentReconciler) renderRollout(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	prof engine.Profile,
	targetSpec inferencev1alpha1.ModelDeploymentSpec,
	target string,
	plan rollout,
) (desiredChildren, error) {
	primarySpec, err := r.specForRevision(ctx, md, target, plan.PrimaryRevision, targetSpec)
	if err != nil {
		return desiredChildren{}, err
	}

	primary := variantPlan{
		Spec:     primarySpec,
		Revision: plan.PrimaryRevision,
		Replicas: plan.PrimaryReplicas,
	}

	if plan.CanaryRevision == "" || plan.CanaryReplicas <= 0 {
		return buildChildren(md, prof, primary, nil, r.renderOptions())
	}

	canarySpec, err := r.specForRevision(ctx, md, target, plan.CanaryRevision, targetSpec)
	if err != nil {
		return desiredChildren{}, err
	}

	canary := variantPlan{
		Spec:     canarySpec,
		Revision: plan.CanaryRevision,
		Replicas: plan.CanaryReplicas,
	}
	return buildChildren(md, prof, primary, &canary, r.renderOptions())
}

// specForRevision reconstructs the spec a revision was recorded with.
//
// The reconstruction is a MERGE, never a wholesale replacement, and the
// distinction is load-bearing. A v2 ControllerRevision stores only the fields
// that define a workload — model, engine, startup timeout and shim — so
// everything else comes back zero. Assigning it directly would set Replicas to
// nil, undoing whatever an autoscaler had decided, and clear the rollout
// settings, turning a rollback into an unintended scale-down at the worst
// possible moment.
//
// Missing history is an error. Substituting the live candidate would overwrite
// the stable workload under its old revision label. Returning before applying
// children preserves existing workloads while history is restored.
func (r *ModelDeploymentReconciler) specForRevision(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	target, want string,
	resolvedTarget ...inferencev1alpha1.ModelDeploymentSpec,
) (inferencev1alpha1.ModelDeploymentSpec, error) {
	if want == "" || want == target {
		if len(resolvedTarget) > 0 {
			return mergeWorkloadSpec(md.Spec, resolvedTarget[0], false), nil
		}
		return md.Spec, nil
	}

	if r.revisions == nil {
		r.revisions = &revision.Recorder{Client: r.Client, Scheme: r.Scheme}
	}

	cr, err := r.revisions.Get(ctx, md, want)
	if apierrors.IsNotFound(err) {
		return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf(
			"revision history %s is missing; refusing to replace the stable workload with the live candidate: %w", want, err)
	}
	if err != nil {
		return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf("reading revision %s: %w", want, err)
	}

	stored, err := revision.Decode(cr.Data.Raw)
	if err != nil {
		return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf("decoding revision %s: %w", want, err)
	}
	if stored.Legacy() {
		legacyType := stored.Spec.Engine.Type
		if legacyType == "" {
			legacyType = inferencev1alpha1.EngineLlamaCPP
		}
		legacyProfile, profileErr := engine.Get(legacyType)
		if profileErr != nil {
			return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf(
				"resolving legacy revision %s: %w", want, profileErr)
		}
		stored.Spec, profileErr = r.resolveLegacyRevisionSpec(ctx, md, want, stored.Spec, legacyProfile)
		if profileErr != nil {
			return inferencev1alpha1.ModelDeploymentSpec{}, fmt.Errorf(
				"resolving legacy revision %s: %w", want, profileErr)
		}
	}
	return mergeWorkloadSpec(md.Spec, stored.Spec, stored.Legacy()), nil
}

// deploymentFor reads one child Deployment by name, tolerating its absence.
func (r *ModelDeploymentReconciler) deploymentFor(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	name string,
) (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: md.Namespace, Name: name}

	switch err := r.Get(ctx, key, &dep); {
	case err == nil:
		return &dep, nil
	case apierrors.IsNotFound(err):
		// Just applied it, or it has not been created yet; the cache has not
		// caught up. Status reports Progressing and the watch brings us back.
		return nil, nil
	default:
		return nil, fmt.Errorf("reading Deployment %s: %w", name, err)
	}
}

// recordRevision persists the current spec revision and prunes old history.
func (r *ModelDeploymentReconciler) recordRevision(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	rev string,
	payload ...[]byte,
) error {
	if r.revisions == nil {
		r.revisions = &revision.Recorder{Client: r.Client, Scheme: r.Scheme}
	}

	// The event keys off Record's `created` return, not off a preceding read.
	// Reads go through the informer cache, which lags our own writes, so a
	// cache-based check reports "new" on every reconcile until it catches up —
	// which is exactly how an audit trail fills with duplicate entries for one
	// revision.
	_, created, err := r.revisions.Record(ctx, md, rev, payload...)
	if err != nil {
		return err
	}

	if created {
		r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonRevisionCreated,
			"RecordRevision", fmt.Sprintf("Recorded revision %s", rev))
	}

	// A new spec can arrive while a candidate is active. Retain every recorded
	// identity needed to resume or roll back that rollout, even if its revisions
	// are older than the normal history limit.
	keep := []string{rev, md.Status.StableRevision, md.Status.LastGoodRevision}
	if cs := md.Status.Canary; cs != nil {
		keep = append(keep, cs.Revision, cs.StableRevision, cs.FailedRevision)
	}
	return r.revisions.Prune(ctx, md, revisionHistoryLimit, keep...)
}

// applyChildren writes the desired child objects with Server-Side Apply.
//
// SSA rather than create-or-update: the controller declares exactly the fields
// it owns, and the API server reconciles ownership. That makes coexistence with
// other writers explicit instead of something this controller has to
// hand-implement, and it removes the read-modify-write conflict retries that
// CreateOrUpdate needs.
//
// ForceOwnership is set because on conflict this controller is the authority
// for the fields it declares — that is what "the spec is the source of truth"
// means in practice.
func (r *ModelDeploymentReconciler) applyChildren(ctx context.Context, desired desiredChildren) error {
	opts := []client.ApplyOption{
		client.FieldOwner(naming.FieldManager),
		client.ForceOwnership,
	}

	if err := r.Apply(ctx, desired.Deployment, opts...); err != nil {
		return fmt.Errorf("applying Deployment: %w", err)
	}
	if err := r.Apply(ctx, desired.Service, opts...); err != nil {
		return fmt.Errorf("applying Service: %w", err)
	}
	if err := r.Apply(ctx, desired.MetricsService, opts...); err != nil {
		return fmt.Errorf("applying metrics Service: %w", err)
	}
	if desired.CanaryDeployment != nil {
		if err := r.Apply(ctx, desired.CanaryDeployment, opts...); err != nil {
			return fmt.Errorf("applying canary Deployment: %w", err)
		}
	}
	return nil
}

// observe reads back the cluster state the status will be computed from.
func (r *ModelDeploymentReconciler) observe(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	rev string,
	plan rollout,
) (observed, error) {
	selector, err := serviceSelectorString(md)
	if err != nil {
		return observed{}, err
	}

	obs := observed{
		DesiredReplicas: replicasFor(md),
		Revision:        rev,
		PrimaryRevision: plan.PrimaryRevision,
		Selector:        selector,
		Endpoint:        endpointFor(md),
		Rollout:         plan,
	}

	obs.Deployment, err = r.deploymentFor(ctx, md, naming.PrimaryDeployment(md.Name))
	if err != nil {
		return observed{}, err
	}
	obs.CanaryDeployment, err = r.canaryDeploymentFor(ctx, md)
	if err != nil {
		return observed{}, err
	}

	return obs, nil
}

// updateStatus computes and writes status, skipping the write when nothing
// changed.
//
// The no-op check is not an optimisation. Writing an identical status still
// bumps resourceVersion, which fires the watch, which triggers another
// reconcile — a hot loop that never converges and is invisible until it is
// saturating the API server.
func (r *ModelDeploymentReconciler) updateStatus(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	obs observed,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest inferencev1alpha1.ModelDeployment
		if err := r.Get(ctx, client.ObjectKeyFromObject(md), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		// Read BEFORE computeStatus, not after. computeStatus copies the
		// condition slice so this is no longer load-bearing, but reading a
		// "before" value after the call that computes the "after" one is the
		// shape of the bug rather than a detail of it — the ordering is the
		// thing that makes the transition detectable.
		wasReady := isConditionTrue(latest.Status.Conditions, inferencev1alpha1.ConditionReady)

		newStatus := computeStatus(&latest, obs, latest.Status)
		if apiequality.Semantic.DeepEqual(latest.Status, newStatus) {
			return nil
		}

		nowReady := isConditionTrue(newStatus.Conditions, inferencev1alpha1.ConditionReady)

		latest.Status = newStatus
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}

		if !wasReady && nowReady {
			r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonRolloutComplete,
				"Rollout", fmt.Sprintf("Revision %s is serving", obs.Revision))
		}
		return nil
	})
}

// failTerminally records an unusable spec and stops retrying.
//
// reconcile.TerminalError suppresses the exponential-backoff requeue while
// still logging and counting the failure. Without it the controller would spin
// forever on input that no amount of retrying can fix, burning API quota and
// drowning the logs.
func (r *ModelDeploymentReconciler) failTerminally(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	cause error,
) (ctrl.Result, error) {
	msg := cause.Error()

	r.event(md, corev1.EventTypeWarning, inferencev1alpha1.EventReasonInvalidSpec, "Validate", msg)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest inferencev1alpha1.ModelDeployment
		if err := r.Get(ctx, client.ObjectKeyFromObject(md), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		status := latest.Status
		status.ObservedGeneration = latest.Generation
		setInvalidSpec(&status, latest.Generation, msg)
		if apiequality.Semantic.DeepEqual(latest.Status, status) {
			return nil
		}
		latest.Status = status
		return r.Status().Update(ctx, &latest)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, reconcile.TerminalError(cause)
}

// event records a Kubernetes event, tolerating a nil recorder so that unit
// tests can construct a reconciler without one.
//
// This uses the events.k8s.io recorder rather than the legacy core/v1 one.
// controller-runtime deprecated GetEventRecorderFor in favour of
// GetEventRecorder for exactly this reason, and the newer API takes an extra
// `action` argument: the verb describing what the controller did, which is
// indexed separately from `reason` so that events can be filtered by operation
// as well as by outcome.
func (r *ModelDeploymentReconciler) event(
	md *inferencev1alpha1.ModelDeployment,
	eventType, reason, action, message string,
) {
	if r.Recorder == nil {
		return
	}
	// `related` is nil: these events concern the ModelDeployment itself, not a
	// relationship between it and a second object.
	r.Recorder.Eventf(md, nil, eventType, reason, action, "%s", message)
}

// eventOnce emits an event only when a condition's reason is about to CHANGE.
//
// The de-duplication keys off the current status rather than a field on the
// reconciler, which matters twice over: it fires once per transition rather
// than once per process lifetime, and a leader-election handover does not
// replay the backlog. Events are rate-limited by the API server anyway, but a
// controller that re-emits the same warning every fifteen seconds trains people
// to filter its events out — and these are the events the canary dashboard's
// annotations are sourced from, so a duplicate is a second vertical line on a
// graph at a moment when nothing happened.
func (r *ModelDeploymentReconciler) eventOnce(
	md *inferencev1alpha1.ModelDeployment,
	conditionType, reason string,
	eventType, eventReason, action, message string,
) {
	if conditionReason(md.Status.Conditions, conditionType) == reason {
		return
	}
	r.event(md, eventType, eventReason, action, message)
}

// noteMetricsUnavailable emits an event the first time metric collection
// becomes unavailable for a resource.
func (r *ModelDeploymentReconciler) noteMetricsUnavailable(
	md *inferencev1alpha1.ModelDeployment,
	verdict metricsVerdict,
) {
	if verdict.Status == metav1.ConditionTrue {
		return
	}
	r.eventOnce(md, inferencev1alpha1.ConditionMetricsRegistered, verdict.Reason,
		corev1.EventTypeWarning, inferencev1alpha1.EventReasonMetricsUnavailable,
		"RegisterMetrics", verdict.Message)
}

// conditionReason returns the reason of a condition, or "" when absent.
func conditionReason(conds []metav1.Condition, condType string) string {
	for i := range conds {
		if conds[i].Type == condType {
			return conds[i].Reason
		}
	}
	return ""
}

// isConditionTrue reports whether a condition of the given type is True.
func isConditionTrue(conds []metav1.Condition, condType string) bool {
	for i := range conds {
		if conds[i].Type == condType {
			return conds[i].Status == metav1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager wires the controller into the manager.
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("modeldeployment-controller")
	}
	r.revisions = &revision.Recorder{Client: r.Client, Scheme: r.Scheme}

	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelDeployment{},
			// Filter status-only self-writes, which would otherwise make every
			// status update trigger another reconcile. Annotations are included
			// because they are how out-of-band operator actions (promote,
			// abort) will be signalled, and those must wake the controller.
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			))).
		//
		// Deliberately NO predicate on the owned Deployment.
		//
		// GenerationChangedPredicate here would be a serious bug: a Deployment's
		// generation does not change when its STATUS does, and status is
		// precisely what we need — readyReplicas moving, or the rollout being
		// declared stalled. Filtering on generation would mean the controller
		// never noticed its workload becoming healthy.
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("modeldeployment").
		Complete(r)
}
