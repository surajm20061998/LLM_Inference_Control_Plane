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

	inferencev1alpha1 "github.com/surajmishra/llmcp/api/v1alpha1"
	"github.com/surajmishra/llmcp/internal/engine"
	"github.com/surajmishra/llmcp/internal/naming"
	"github.com/surajmishra/llmcp/internal/revision"
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
// computeStatus, revision.Hash, engine.Profile.Build) that take no client and
// no clock, so the bulk of the test suite needs neither an API server nor
// wall-clock waiting.
type ModelDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Clock is injected so that anything time-dependent is deterministic under
	// test. Production passes a real clock; tests pass a fake one and step it.
	Clock clock.PassiveClock

	// revisions persists rollout history. Built lazily from Client so that a
	// zero-value reconciler constructed in a test still works.
	revisions *revision.Recorder
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
		// Not found is the normal path after a delete: every child carries an
		// owner reference, so garbage collection has already cleaned up.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A resource being deleted needs nothing from us. There is no finalizer by
	// design, so we simply stop touching it.
	if !md.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	prof, err := engine.Get(md.Spec.Engine.Type)
	if err != nil {
		return r.failTerminally(ctx, &md, err)
	}
	if err := prof.Validate(&md.Spec); err != nil {
		return r.failTerminally(ctx, &md, err)
	}

	rev := revision.Hash(&md.Spec)

	if err := r.recordRevision(ctx, &md, rev); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording revision: %w", err)
	}

	desired, err := buildChildren(&md, prof, rev)
	if err != nil {
		// A build failure is a spec the controller cannot render. Retrying
		// cannot fix it, so do not spin.
		return r.failTerminally(ctx, &md, err)
	}

	if err := r.applyChildren(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}

	obs, err := r.observe(ctx, &md, rev)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, &md, obs); err != nil {
		return ctrl.Result{}, err
	}

	log.V(1).Info("reconciled", "revision", rev, "ready", obs.Deployment != nil)

	// No RequeueAfter: the Deployment is watched via Owns(), so every change to
	// its status — including the Deployment controller declaring the rollout
	// stalled — wakes this reconciler. Polling on a timer would be strictly
	// worse: slower to react and more API traffic at rest.
	return ctrl.Result{}, nil
}

// recordRevision persists the current spec revision and prunes old history.
func (r *ModelDeploymentReconciler) recordRevision(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	rev string,
) error {
	if r.revisions == nil {
		r.revisions = &revision.Recorder{Client: r.Client, Scheme: r.Scheme}
	}

	// The event keys off Record's `created` return, not off a preceding read.
	// Reads go through the informer cache, which lags our own writes, so a
	// cache-based check reports "new" on every reconcile until it catches up —
	// which is exactly how an audit trail fills with duplicate entries for one
	// revision.
	_, created, err := r.revisions.Record(ctx, md, rev)
	if err != nil {
		return err
	}

	if created {
		r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonRevisionCreated,
			"RecordRevision", fmt.Sprintf("Recorded revision %s", rev))
	}

	// Never prune the revision currently serving or the rollback target.
	return r.revisions.Prune(ctx, md, revisionHistoryLimit, rev, md.Status.LastGoodRevision)
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
	return nil
}

// observe reads back the cluster state the status will be computed from.
func (r *ModelDeploymentReconciler) observe(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	rev string,
) (observed, error) {
	selector, err := serviceSelectorString(md)
	if err != nil {
		return observed{}, err
	}

	obs := observed{
		DesiredReplicas: replicasFor(md),
		Revision:        rev,
		Selector:        selector,
		Endpoint:        endpointFor(md),
	}

	var dep appsv1.Deployment
	key := types.NamespacedName{
		Namespace: md.Namespace,
		Name:      naming.PrimaryDeployment(md.Name),
	}
	switch err := r.Get(ctx, key, &dep); {
	case err == nil:
		obs.Deployment = &dep
	case apierrors.IsNotFound(err):
		// Just applied it; the cache has not caught up. Status will report
		// Progressing and the watch will bring us straight back.
	default:
		return observed{}, fmt.Errorf("reading Deployment: %w", err)
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

		newStatus := computeStatus(&latest, obs, latest.Status)
		if apiequality.Semantic.DeepEqual(latest.Status, newStatus) {
			return nil
		}

		wasReady := isConditionTrue(latest.Status.Conditions, inferencev1alpha1.ConditionReady)
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
