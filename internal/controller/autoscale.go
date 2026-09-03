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
	"math"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/autoscale"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
)

// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch

// The autoscaling read permission above exists for ONE purpose: detecting that
// an external HorizontalPodAutoscaler targets this ModelDeployment while the
// built-in autoscaler is also enabled. Two controllers writing .spec.replicas
// from different signals do not average out — they fight, and the fleet
// oscillates at the rate of the faster one. The symptom looks like a flapping
// workload rather than a configuration mistake, which is why one cached List is
// worth paying for.

// autoscaleVerdict is the outcome of one autoscaling pass.
//
// A value rather than an error, for the same reason the metrics verdict is:
// most of its outcomes are not failures. "Autoscaling is off" and "the provider
// is unreachable so the fleet is frozen" are both states the resource should
// report rather than reconcile failures that hide behind a backoff.
type autoscaleVerdict struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string

	// Autoscaling is the status block to publish, or nil when disabled.
	Autoscaling *inferencev1alpha1.AutoscalingStatus

	// RequeueAfter is the poll interval, so the loop keeps sampling even when
	// nothing in the cluster changes.
	//
	// This is the one place in the operator that genuinely needs a timer.
	// Everything else is driven by watches, but "the load went up" is not an
	// event any Kubernetes object emits — the only way to notice is to look.
	RequeueAfter time.Duration
}

// reconcileAutoscaling samples the metric, decides a replica count, and writes
// it.
//
// Ordering note: this runs BEFORE the rollout is planned, so a scale decision
// and the canary split that distributes it happen in the same pass. Running it
// afterwards would leave the fleet one reconcile behind its own decision, which
// during a canary means one full analysis window at the wrong size.
func (r *ModelDeploymentReconciler) reconcileAutoscaling(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	primaryDep, canaryDep *appsv1.Deployment,
) (autoscaleVerdict, error) {
	spec := md.Spec.Autoscaling

	if !spec.AutoscalingEnabled() {
		return autoscaleVerdict{
			Status: metav1.ConditionFalse,
			Reason: inferencev1alpha1.ReasonAutoscalingDisabled,
			Message: "Replica count is owned outside this operator: set spec.autoscaling.mode " +
				"to Builtin, or drive .spec.replicas with kubectl scale or a HorizontalPodAutoscaler",
		}, nil
	}

	// The conflict check runs first and short-circuits. Sampling a metric and
	// writing a replica count while another controller is doing the same thing
	// would make the operator a participant in the oscillation rather than the
	// thing that reports it.
	if conflict, err := r.externalScalerConflict(ctx, md); err != nil {
		return autoscaleVerdict{
			Status:  metav1.ConditionUnknown,
			Reason:  inferencev1alpha1.ReasonReconciling,
			Message: "Could not check for a conflicting HorizontalPodAutoscaler: " + err.Error(),
		}, err
	} else if conflict != "" {
		msg := "HorizontalPodAutoscaler " + conflict + " also targets this ModelDeployment while " +
			"spec.autoscaling.mode is Builtin. Two controllers writing .spec.replicas from different " +
			"signals will fight; the built-in autoscaler is standing down. Delete the HPA, or set mode to Off."
		r.eventOnce(md, inferencev1alpha1.ConditionAutoscalingReady,
			inferencev1alpha1.ReasonAutoscalingConflict,
			corev1.EventTypeWarning, inferencev1alpha1.EventReasonAutoscalingConflict, "Autoscale", msg)

		return autoscaleVerdict{
			Status:       metav1.ConditionFalse,
			Reason:       inferencev1alpha1.ReasonAutoscalingConflict,
			Message:      msg,
			RequeueAfter: spec.ResolvedPollInterval(),
		}, nil
	}

	obs := r.sampleAutoscalingMetric(ctx, md)

	in := autoscale.Input{
		Now:           r.now(),
		Current:       replicasFor(md),
		ReadyReplicas: readyAcrossVariants(primaryDep, canaryDep),
		Observation:   obs,
		Config: autoscale.Config{
			MinReplicas:            spec.MinReplicas,
			MaxReplicas:            spec.MaxReplicas,
			Target:                 spec.ResolvedTarget(),
			Tolerance:              spec.ResolvedTolerance(),
			ScaleUpStabilization:   spec.ResolvedScaleUpStabilization(),
			ScaleDownStabilization: spec.ResolvedScaleDownStabilization(),
		},
		History: recommendationsFrom(md.Status.Autoscaling),
	}

	out := autoscale.Recommend(in)

	observability.RecordAutoscaleDecision(resourceKeyFor(md), out.Replicas)

	verdict := autoscaleVerdict{
		Status:       metav1.ConditionTrue,
		Reason:       autoscalingReason(out.Reason),
		Message:      out.Message,
		RequeueAfter: spec.ResolvedPollInterval(),
		Autoscaling:  autoscalingStatusFrom(out, obs, md.Status.Autoscaling, r.now()),
	}

	if !out.Valid {
		// FALSE, not Unknown. A frozen fleet is not a neutral state: demand can
		// climb underneath it indefinitely and nothing else in the status says
		// so.
		verdict.Status = metav1.ConditionFalse
		verdict.Reason = inferencev1alpha1.ReasonAutoscalingNoMetrics
		r.eventOnce(md, inferencev1alpha1.ConditionAutoscalingReady,
			inferencev1alpha1.ReasonAutoscalingNoMetrics,
			corev1.EventTypeWarning, inferencev1alpha1.EventReasonAutoscalingNoMetrics,
			"Autoscale", out.Message)
	}

	if !out.Changed {
		return verdict, nil
	}

	if err := r.applyReplicas(ctx, md, out.Replicas); err != nil {
		return verdict, err
	}

	logf.FromContext(ctx).Info("autoscaled",
		"from", in.Current, "to", out.Replicas, "perPod", out.PerPod, "reason", out.Reason)

	switch out.Direction {
	case autoscale.DirectionUp:
		r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonScaledUp, "Autoscale", out.Message)
	case autoscale.DirectionDown:
		r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonScaledDown, "Autoscale", out.Message)
	}

	now := metav1.NewTime(r.now())
	verdict.Autoscaling.LastScaleTime = &now

	return verdict, nil
}

// sampleAutoscalingMetric reads the fleet-wide signal.
//
// Every failure mode collapses into `Valid: false` with a reason, and none of
// them produces a number. That is the whole discipline: an autoscaler that
// treats "I could not measure" as "the measurement is zero" scales a fleet to
// its floor during a monitoring outage, which turns a Prometheus incident into
// a serving incident.
func (r *ModelDeploymentReconciler) sampleAutoscalingMetric(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) autoscale.Observation {
	spec := md.Spec.Autoscaling

	provider := r.autoscalingProviderFor(md)
	if provider == nil {
		return autoscale.Observation{
			Reason: "no metric provider is configured: set spec.autoscaling.provider.address",
		}
	}

	query, err := analysis.AutoscalingQuery(spec.ResolvedMetric(), analysis.QueryContext{
		Model:  md.Spec.Model.Name,
		Window: analysis.PromDuration(spec.ResolvedWindow()),
	})
	if err != nil {
		return autoscale.Observation{Reason: "building the query: " + err.Error()}
	}

	sample, err := provider.Query(ctx, query)
	switch {
	case err != nil:
		return autoscale.Observation{Reason: "querying the metric provider failed: " + err.Error()}
	case sample.Count == 0:
		return autoscale.Observation{
			Reason: "the query matched no series; this is not a measurement of zero, it is the " +
				"absence of one — check that the shim is injected and being scraped",
		}
	case sample.Count > 1:
		return autoscale.Observation{
			Reason: fmt.Sprintf("the query returned %d series; it must aggregate to exactly one",
				sample.Count),
		}
	case math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0):
		return autoscale.Observation{
			Reason: "the query returned " + analysis.LiteralNaN + " or an infinity",
		}
	}

	return autoscale.Observation{Value: sample.Value, Valid: true}
}

// autoscalingProviderFor builds the metric provider for the autoscaler.
//
// An injected Provider wins, which is what lets an envtest suite drive a
// scripted load curve — rising, plateau, falling — and assert that the fleet
// grows promptly and shrinks only after the stabilization window. That sequence
// cannot be produced against a live Prometheus on demand.
func (r *ModelDeploymentReconciler) autoscalingProviderFor(
	md *inferencev1alpha1.ModelDeployment,
) analysis.Provider {
	if r.Provider != nil {
		return r.Provider
	}
	spec := md.Spec.Autoscaling
	if spec == nil || spec.Provider.Address == "" {
		return nil
	}
	return &analysis.PrometheusProvider{
		Address: spec.Provider.Address,
		Timeout: spec.ResolvedProviderTimeout(),
	}
}

// applyReplicas writes the autoscaler's decision to .spec.replicas.
//
// # Why the spec and not the child Deployments
//
// Because .spec.replicas is the /scale subresource's target, and that makes the
// built-in autoscaler and an external HPA genuinely interchangeable: both write
// the same field, the controller reconciles it the same way, and `kubectl get
// modeldeployment` shows one number that means what it says. Writing the child
// Deployments directly would leave .spec.replicas stale, so `kubectl scale`
// would report a count the cluster is not running.
//
// # Why a merge patch and not an Update
//
// An Update sends the whole object, so it would clobber any concurrent edit —
// including a user changing the model image in the same second. The patch names
// one field and touches nothing else. It bumps .metadata.generation, which
// wakes the controller again; the next pass recomputes the same recommendation,
// finds nothing changed, and stops. That convergence is what the tolerance band
// and the stabilization windows are for.
func (r *ModelDeploymentReconciler) applyReplicas(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	replicas int32,
) error {
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)

	if err := r.Patch(ctx, md, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("scaling to %d replicas: %w", replicas, err)
	}

	// Keep the in-memory object consistent with what was just written, so the
	// rollout planned later in this same pass distributes the NEW total rather
	// than the old one.
	md.Spec.Replicas = &replicas
	return nil
}

// externalScalerConflict returns the name of an HPA targeting this
// ModelDeployment, or "" when there is none.
//
// The List is served from the manager's cache, so this costs no API round trip
// in steady state. Scoped to the ModelDeployment's own namespace because an HPA
// can only target a workload in its own namespace.
func (r *ModelDeploymentReconciler) externalScalerConflict(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) (string, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := r.List(ctx, &list, client.InNamespace(md.Namespace)); err != nil {
		if apimeta.IsNoMatchError(err) {
			// autoscaling/v2 is a core API and is present on every supported
			// cluster, so this is close to unreachable. Tolerating it anyway
			// costs one branch and means an exotic or partially-installed
			// cluster loses the conflict CHECK rather than the autoscaler.
			return "", nil
		}
		return "", fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}

	for i := range list.Items {
		ref := list.Items[i].Spec.ScaleTargetRef
		if ref.Kind == kindModelDeployment &&
			ref.Name == md.Name &&
			sameGroup(ref.APIVersion, inferencev1alpha1.GroupVersion.Group) {
			return list.Items[i].Name, nil
		}
	}
	return "", nil
}

// sameGroup reports whether an apiVersion belongs to the given API group.
//
// Compared by GROUP rather than by the full apiVersion string, so that an HPA
// written against a future v1beta1 of this CRD is still recognised as a
// conflict. It is the same resource being scaled either way.
func sameGroup(apiVersion, group string) bool {
	for i := range apiVersion {
		if apiVersion[i] == '/' {
			return apiVersion[:i] == group
		}
	}
	return false
}

// recommendationsFrom restores the stabilization history from status.
func recommendationsFrom(status *inferencev1alpha1.AutoscalingStatus) []autoscale.Recommendation {
	if status == nil {
		return nil
	}
	out := make([]autoscale.Recommendation, 0, len(status.Recommendations))
	for _, r := range status.Recommendations {
		out = append(out, autoscale.Recommendation{Time: r.Time.Time, Replicas: r.Replicas})
	}
	return out
}

// autoscalingStatusFrom renders the autoscaler's decision for status.
func autoscalingStatusFrom(
	out autoscale.Output,
	obs autoscale.Observation,
	prev *inferencev1alpha1.AutoscalingStatus,
	_ time.Time,
) *inferencev1alpha1.AutoscalingStatus {
	status := &inferencev1alpha1.AutoscalingStatus{
		DesiredReplicas: out.Replicas,
		Message:         out.Message,
	}

	if obs.Valid {
		status.CurrentMetric = formatMetric(obs.Value)
		status.CurrentMetricPerPod = formatMetric(out.PerPod)
	}

	// Carried forward rather than recomputed: it records when the fleet last
	// actually changed size, which is information no single pass can derive.
	if prev != nil {
		status.LastScaleTime = prev.LastScaleTime
	}

	status.Recommendations = make([]inferencev1alpha1.ScaleRecommendation, 0, len(out.History))
	for _, r := range out.History {
		status.Recommendations = append(status.Recommendations,
			inferencev1alpha1.ScaleRecommendation{
				Time:     metav1.NewTime(r.Time),
				Replicas: r.Replicas,
			})
	}

	return status
}

// autoscalingReason maps an autoscale decision reason onto a condition reason.
func autoscalingReason(reason string) string {
	switch reason {
	case autoscale.ReasonNoMetrics:
		return inferencev1alpha1.ReasonAutoscalingNoMetrics
	case autoscale.ReasonScaleUp:
		return inferencev1alpha1.ReasonScaledUp
	case autoscale.ReasonScaleDown:
		return inferencev1alpha1.ReasonScaledDown
	default:
		return inferencev1alpha1.ReasonAutoscalingActive
	}
}

// readyAcrossVariants counts ready pods over the whole fleet.
//
// Both variants, because the autoscaler sizes the fleet and the rollout splits
// it. Counting only the primary during a canary would under-report the serving
// capacity by exactly the canary's share.
func readyAcrossVariants(primary, canary *appsv1.Deployment) int32 {
	var total int32
	for _, dep := range []*appsv1.Deployment{primary, canary} {
		if dep != nil {
			total += dep.Status.ReadyReplicas
		}
	}
	return total
}

// formatMetric renders an observed value for status.
func formatMetric(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}
