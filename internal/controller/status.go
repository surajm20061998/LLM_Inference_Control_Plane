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
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/canary"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// observed is the cluster state one reconcile pass acted on.
//
// Collecting it into a struct keeps computeStatus a pure function of its
// inputs, which is what lets the whole status surface be table-tested without
// an API server.
type observed struct {
	// Deployment is the primary variant's Deployment, or nil if it does not
	// exist yet.
	Deployment *appsv1.Deployment

	// CanaryDeployment is the canary variant's, or nil when none is running.
	CanaryDeployment *appsv1.Deployment

	// DesiredReplicas is the total desired across all variants.
	DesiredReplicas int32

	// Revision is the TARGET revision hash — what the current spec describes.
	Revision string

	// PrimaryRevision is what the primary Deployment is actually being run at.
	//
	// During a canary these differ: the primary keeps serving the last
	// known-good revision while the canary carries the target. Conflating them
	// would let lastGoodRevision advance to a revision that has only ever run
	// as a canary, which is precisely the revision a rollback needs to escape.
	PrimaryRevision string

	// Selector is the serialized /scale label selector.
	Selector string

	// Endpoint is the in-cluster OpenAI-compatible base URL.
	Endpoint string

	// Rollout is the plan this pass acted on.
	Rollout rollout

	// Scaling is the autoscaler's verdict for this pass.
	Scaling autoscaleVerdict

	// Metrics is the verdict from reconciling metric collection. The zero value
	// means "not evaluated", which is reported as Unknown rather than False —
	// a consumer must be able to tell "we have not looked" from "we looked and
	// there is nothing".
	Metrics metricsVerdict
}

// computeStatus derives the full status from observed cluster state.
//
// It takes the previous status so that fields which are only advanced on
// success — notably lastGoodRevision — are carried forward rather than
// recomputed. It never reads the clock: condition timestamps are managed by
// apimeta.SetStatusCondition, which only stamps a new time when a condition
// actually transitions.
func computeStatus(
	md *inferencev1alpha1.ModelDeployment,
	obs observed,
	prev inferencev1alpha1.ModelDeploymentStatus,
) inferencev1alpha1.ModelDeploymentStatus {
	// An unset PrimaryRevision means "the primary is running the target", which
	// is the case for every non-canary rollout. Normalising once here keeps
	// every use below from repeating the fallback — and, more usefully, keeps a
	// caller that has no canary from having to know the field exists.
	primaryRevision := firstNonEmpty(obs.PrimaryRevision, obs.Revision)

	status := inferencev1alpha1.ModelDeploymentStatus{
		ObservedGeneration: md.Generation,
		Selector:           obs.Selector,
		Endpoint:           obs.Endpoint,
		// The revision actually serving as primary, which during a canary is
		// NOT the target.
		StableRevision:   primaryRevision,
		LastGoodRevision: prev.LastGoodRevision,
		Conditions:       prev.Conditions,
	}

	// Replica counters SUM both variants.
	//
	// This is the /scale subresource's contract — .spec.replicas is the total
	// across all variants, so .status.replicas must be too — and an HPA divides
	// its metric by this number. Reporting only the primary's during a canary
	// would make the autoscaler compute its target from two thirds of the
	// fleet and scale up to compensate for capacity that already exists.
	for _, dep := range []*appsv1.Deployment{obs.Deployment, obs.CanaryDeployment} {
		if dep == nil {
			continue
		}
		status.Replicas += dep.Status.Replicas
		status.ReadyReplicas += dep.Status.ReadyReplicas
		status.AvailableReplicas += dep.Status.AvailableReplicas
	}

	// updatedReplicas counts pods on the TARGET revision, which is the canary's
	// during a rollout and the primary's otherwise. Summing both would report a
	// rollout as complete while most pods still run the old version.
	if obs.Deployment != nil && primaryRevision == obs.Revision {
		status.UpdatedReplicas += obs.Deployment.Status.UpdatedReplicas
	}
	if obs.CanaryDeployment != nil {
		status.UpdatedReplicas += obs.CanaryDeployment.Status.UpdatedReplicas
	}

	setSpecValid(&status, md.Generation)
	setMetricsRegistered(&status, obs.Metrics, md.Generation)
	available := setAvailable(&status, obs, md.Generation)
	progressing, stalled := setProgressing(&status, obs, md.Generation)
	setModelReady(md, &status, available, progressing, md.Generation)

	status.Canary = canaryStatusFrom(obs.Rollout.Output, obs.Rollout.Round, prev.Canary)
	status.Autoscaling = obs.Scaling.Autoscaling
	canaryActive := setCanaryHealthy(&status, obs, md.Generation)
	setTrafficRoutingReady(&status, obs, md.Generation)
	setAutoscalingReady(&status, obs.Scaling, md.Generation)

	// Only a revision that actually reached availability is a safe rollback
	// target. Recording it any earlier would let a broken revision become the
	// thing a future rollback reverts *to*.
	//
	// The !stalled term is not redundant with !progressing. A rollout that
	// blew its deadline reports Progressing=False, and if any old-revision
	// replica is still passing readiness it also reports Available=True — so
	// without this term a stalled rollout would bank the very revision that
	// failed, and an automatic rollback would then "recover" to the broken
	// version it was trying to escape.
	//
	// The !canaryActive term is the canary-era addition. While a canary is in
	// flight the primary is serving the OLD revision perfectly well, so
	// available is true and progressing is false — and without this term the
	// controller would re-bank the revision it already had, harmlessly, on
	// every reconcile, while the actual promotion decision was still pending.
	// More importantly it keeps lastGoodRevision from ever advancing to a
	// revision that has only run as a canary.
	if available && !progressing && !stalled && !canaryActive && primaryRevision != "" {
		status.LastGoodRevision = primaryRevision
	}

	setReady(&status, available, progressing, md.Generation)
	status.Phase = derivePhase(available, progressing, stalled, status.LastGoodRevision, obs.Rollout)

	return status
}

// setCanaryHealthy reports the most recent analysis verdict, and returns
// whether a canary is in flight.
//
// It is Unknown — not True — when nothing is running. True would claim a
// healthy canary exists, and `kubectl wait --for=condition=CanaryHealthy` would
// then return immediately against a ModelDeployment that has never canaried
// anything, which is the sort of green tick that makes a whole status surface
// untrustworthy.
func setCanaryHealthy(
	status *inferencev1alpha1.ModelDeploymentStatus,
	obs observed,
	generation int64,
) bool {
	out := obs.Rollout.Output

	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionCanaryHealthy,
		ObservedGeneration: generation,
		Reason:             firstNonEmpty(out.Reason, inferencev1alpha1.ReasonCanaryNotRunning),
		Message:            firstNonEmpty(out.Message, "No canary in flight"),
	}

	active := false

	switch out.Action {
	case canary.ActionRollback:
		cond.Status = metav1.ConditionFalse

	case canary.ActionStart, canary.ActionAdvance, canary.ActionPause:
		cond.Status = metav1.ConditionTrue
		active = out.Action != canary.ActionPause || out.Canary > 0

	case canary.ActionWait:
		active = true
		// A hold is not a pass. It happens for three quite different reasons —
		// a failed check below the threshold, a provider error, or too little
		// traffic — and only the first is a statement about the canary's
		// health. Reporting True for all three would let the condition say the
		// canary is healthy while Prometheus is down.
		switch out.Reason {
		case inferencev1alpha1.ReasonCanaryChecksFailed:
			cond.Status = metav1.ConditionFalse
		case inferencev1alpha1.ReasonAnalysisError, inferencev1alpha1.ReasonAnalysisInconclusive:
			cond.Status = metav1.ConditionUnknown
		default:
			cond.Status = metav1.ConditionTrue
		}

	case canary.ActionPromote:
		cond.Status = metav1.ConditionTrue

	default:
		cond.Status = metav1.ConditionUnknown
	}

	setCondition(status, cond)
	return active
}

// setTrafficRoutingReady reports whether the requested split is in place.
//
// Separate from CanaryHealthy because the two fail for unrelated reasons and
// have unrelated remedies. Analysis can be perfectly healthy while the canary
// is receiving no traffic at all, and that combination looks exactly like
// success — a canary that passes every check having served nothing. Surfacing
// the split as its own condition is what makes that state visible.
func setTrafficRoutingReady(
	status *inferencev1alpha1.ModelDeploymentStatus,
	obs observed,
	generation int64,
) {
	out := obs.Rollout.Output

	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionTrafficRoutingReady,
		ObservedGeneration: generation,
		Status:             metav1.ConditionTrue,
		Reason:             inferencev1alpha1.ReasonTrafficSplit,
	}

	switch {
	case status.Canary == nil || out.Canary == 0:
		cond.Reason = inferencev1alpha1.ReasonCanaryNotRunning
		cond.Message = "All traffic is served by the primary variant"

	case canary.IsQuantized(out.DesiredWeight, out.RealizedWeight):
		// True, not False: the split IS in place, it is simply not the one that
		// was asked for, and that is a property of integer pod counts rather
		// than a fault. Reporting False would make a perfectly working rollout
		// look broken at every step. The reason and message carry the nuance.
		cond.Reason = inferencev1alpha1.ReasonWeightQuantized
		cond.Message = fmt.Sprintf(
			"Requested %d%% to the canary; %d replicas can only express %d%%",
			out.DesiredWeight, out.Primary+out.Canary, out.RealizedWeight)

	default:
		cond.Message = fmt.Sprintf("%d%% of traffic is served by the canary variant", out.RealizedWeight)
	}

	setCondition(status, cond)
}

// setSpecValid marks the spec accepted. Rejection is handled on the error path
// in Reconcile, which sets this condition false and stops.
func setSpecValid(status *inferencev1alpha1.ModelDeploymentStatus, generation int64) {
	setCondition(status, metav1.Condition{
		Type:               inferencev1alpha1.ConditionSpecValid,
		Status:             metav1.ConditionTrue,
		Reason:             inferencev1alpha1.ReasonSpecAccepted,
		Message:            "Specification accepted",
		ObservedGeneration: generation,
	})
}

// setMetricsRegistered reports whether metric collection is wired up.
//
// Deliberately NOT folded into Ready. A ModelDeployment with no metrics serves
// traffic correctly, so failing the readiness rollup over it would make
// `kubectl wait --for=condition=Ready` hang on a cluster that simply has no
// Prometheus — and every e2e test in this repository would then require a
// monitoring stack to pass. Keeping it separate says "serving, but not
// observable" without conflating the two.
func setMetricsRegistered(
	status *inferencev1alpha1.ModelDeploymentStatus,
	verdict metricsVerdict,
	generation int64,
) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionMetricsRegistered,
		Status:             verdict.Status,
		Reason:             verdict.Reason,
		Message:            verdict.Message,
		ObservedGeneration: generation,
	}

	// A zero verdict means nothing evaluated it — a reconciler built without a
	// discovery prober, most often in a unit test. Report Unknown rather than
	// writing an empty reason, which apimeta.SetStatusCondition rejects and
	// which would in any case assert something that was never checked.
	if cond.Status == "" {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = inferencev1alpha1.ReasonReconciling
		cond.Message = "Metric collection has not been evaluated"
	}

	setCondition(status, cond)
}

// setAutoscalingReady reports whether the built-in autoscaler can act.
//
// Deliberately not folded into Ready, on the same reasoning as
// MetricsRegistered: a ModelDeployment whose autoscaler is blind still serves
// traffic correctly at whatever size it currently is, so failing the readiness
// rollup would make `kubectl wait --for=condition=Ready` hang on a cluster that
// simply has no Prometheus. Keeping it separate says "serving, but not
// scaling" without conflating the two.
func setAutoscalingReady(
	status *inferencev1alpha1.ModelDeploymentStatus,
	verdict autoscaleVerdict,
	generation int64,
) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionAutoscalingReady,
		Status:             verdict.Status,
		Reason:             verdict.Reason,
		Message:            verdict.Message,
		ObservedGeneration: generation,
	}

	// A zero verdict means nothing evaluated it — a reconciler assembled
	// without the autoscaling path, which is the shape a focused unit test
	// constructs. Report Unknown rather than writing an empty reason, which
	// apimeta.SetStatusCondition rejects outright.
	if cond.Status == "" {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = inferencev1alpha1.ReasonReconciling
		cond.Message = "Autoscaling has not been evaluated"
	}

	setCondition(status, cond)
}

// setAvailable reports whether the workload is serving, and returns that answer.
//
// Zero replicas is deliberately Available: it is a state the user asked for,
// not a failure, and reporting it as unavailable would make every scale-to-zero
// look like an outage.
func setAvailable(status *inferencev1alpha1.ModelDeploymentStatus, obs observed, generation int64) bool {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionAvailable,
		ObservedGeneration: generation,
	}

	switch {
	case obs.DesiredReplicas == 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonScaledToZero
		cond.Message = "Scaled to zero replicas"
	case obs.Deployment == nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonReconciling
		cond.Message = "Waiting for the serving Deployment to be created"
	case status.ReadyReplicas > 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonMinimumReplicasAvailable
		cond.Message = pluralReplicas(status.ReadyReplicas) + " ready"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonMinimumReplicasUnavailable
		cond.Message = "No replicas are ready"
	}

	setCondition(status, cond)
	return cond.Status == metav1.ConditionTrue
}

// setProgressing reports whether a rollout is in flight, and whether it has
// stalled past its deadline.
//
// The stall verdict is read from the child Deployment rather than recomputed.
// The Deployment controller already tracks progress deadlines correctly, and
// duplicating that logic here would mean two implementations that can disagree.
func setProgressing(
	status *inferencev1alpha1.ModelDeploymentStatus,
	obs observed,
	generation int64,
) (progressing, stalled bool) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionProgressing,
		ObservedGeneration: generation,
	}

	if obs.Deployment == nil {
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonNewRevisionDetected
		cond.Message = "Creating the serving Deployment"
		setCondition(status, cond)
		return true, false
	}

	if depCond := findDeploymentCondition(obs.Deployment, appsv1.DeploymentProgressing); depCond != nil {
		if depCond.Status == corev1.ConditionFalse &&
			depCond.Reason == inferencev1alpha1.ReasonProgressDeadlineExceeded {
			cond.Status = metav1.ConditionFalse
			cond.Reason = inferencev1alpha1.ReasonProgressDeadlineExceeded
			cond.Message = "Rollout has not progressed within spec.rollout.progressDeadline"
			setCondition(status, cond)
			return false, true
		}
	}

	rolloutComplete := status.UpdatedReplicas == obs.DesiredReplicas &&
		status.Replicas == obs.DesiredReplicas &&
		status.AvailableReplicas == obs.DesiredReplicas &&
		obs.Deployment.Status.ObservedGeneration >= obs.Deployment.Generation

	if rolloutComplete {
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonRolloutComplete
		cond.Message = "All replicas are running the target revision"
		setCondition(status, cond)
		return false, false
	}

	cond.Status = metav1.ConditionTrue
	cond.Reason = inferencev1alpha1.ReasonRolloutInProgress
	cond.Message = "Rolling out the target revision"
	setCondition(status, cond)
	return true, false
}

// setModelReady reports whether the engine loaded its model and reported
// healthy. Readiness is the signal: the engine's own /health returns 503 until
// the model is resident, so a ready replica is proof the model loaded.
//
// The False case is split in two, and the split earns its keep once a real
// model is in play. Loading Qwen3 from a local volume takes seconds; pulling it
// from the Hugging Face Hub first takes minutes. For that entire window there
// is legitimately no ready replica, and reporting "EngineUnhealthy" would be
// describing the normal path as a fault — which is how an operator learns to
// ignore the condition that is supposed to tell them something is wrong.
func setModelReady(
	md *inferencev1alpha1.ModelDeployment,
	status *inferencev1alpha1.ModelDeploymentStatus,
	available, progressing bool,
	generation int64,
) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelReady,
		ObservedGeneration: generation,
	}

	switch {
	case available:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonModelResolved
		cond.Message = "Model loaded and the engine is serving"
	case progressing:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonModelLoading
		cond.Message = modelLoadingMessage(md)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonEngineUnhealthy
		cond.Message = "No engine replica has reported healthy"
	}

	setCondition(status, cond)
}

// modelLoadingMessage names what is actually being waited on.
//
// The two sources fail in different places and the message is often the only
// thing pointing at which: an image source that hangs is a registry or a
// missing path, whereas a huggingFace source that hangs is usually egress or a
// rate limit. Naming the source and the artifact turns a support question into
// a one-line answer.
func modelLoadingMessage(md *inferencev1alpha1.ModelDeployment) string {
	switch src := md.Spec.Model.Source; {
	case src.HuggingFace != nil:
		return "Downloading " + src.HuggingFace.File +
			" from Hugging Face repository " + src.HuggingFace.Repo + " and loading it"
	case src.Image != nil:
		return "Loading the model from image " + src.Image.Image
	default:
		return "Waiting for the model to load"
	}
}

// setReady is the top-level rollup that `kubectl wait --for=condition=Ready`
// targets: valid spec, serving, and nothing in flight.
func setReady(status *inferencev1alpha1.ModelDeploymentStatus, available, progressing bool, generation int64) {
	cond := metav1.Condition{
		Type:               inferencev1alpha1.ConditionReady,
		ObservedGeneration: generation,
	}
	switch {
	case available && !progressing:
		cond.Status = metav1.ConditionTrue
		cond.Reason = inferencev1alpha1.ReasonRolloutComplete
		cond.Message = "ModelDeployment is serving the target revision"
	case progressing:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonRolloutInProgress
		cond.Message = "Rollout in progress"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = inferencev1alpha1.ReasonMinimumReplicasUnavailable
		cond.Message = "No replicas are ready"
	}
	setCondition(status, cond)
}

// derivePhase collapses the conditions into the coarse summary shown in
// `kubectl get`. Phase is for humans; automation should read conditions.
//
// It deliberately takes only the condition verdicts, not the observed state
// they were derived from: phase must be a function of the conditions alone, or
// `kubectl get` and the conditions can disagree about the same reconcile.
func derivePhase(
	available, progressing, stalled bool,
	lastGood string,
	plan rollout,
) inferencev1alpha1.Phase {
	// A rollout decision outranks the replica-count view. While a canary is in
	// flight the primary is serving normally, so the availability signals alone
	// would report Available — which is true, and useless: it hides the fact
	// that a release is being evaluated right now.
	switch plan.Output.Action {
	case canary.ActionStart, canary.ActionAdvance:
		return inferencev1alpha1.PhaseCanarying
	case canary.ActionWait:
		if plan.Active {
			return inferencev1alpha1.PhaseCanarying
		}
	case canary.ActionPause:
		return inferencev1alpha1.PhasePaused
	case canary.ActionPromote:
		return inferencev1alpha1.PhasePromoting
	case canary.ActionRollback:
		return inferencev1alpha1.PhaseRollingBack
	}

	switch {
	case stalled:
		return inferencev1alpha1.PhaseDegraded
	case available && !progressing:
		return inferencev1alpha1.PhaseAvailable
	case lastGood == "" && !available:
		// Nothing has ever served, so this is a first rollout rather than a
		// regression. Distinguishing the two matters: Pending is normal,
		// Degraded warrants a page.
		return inferencev1alpha1.PhasePending
	case progressing:
		return inferencev1alpha1.PhaseProgressing
	default:
		return inferencev1alpha1.PhaseDegraded
	}
}

// setInvalidSpec marks the spec unusable. Used on the terminal error path, so
// the rest of the status is left untouched.
func setInvalidSpec(status *inferencev1alpha1.ModelDeploymentStatus, generation int64, msg string) {
	setCondition(status, metav1.Condition{
		Type:               inferencev1alpha1.ConditionSpecValid,
		Status:             metav1.ConditionFalse,
		Reason:             inferencev1alpha1.ReasonInvalidSpec,
		Message:            msg,
		ObservedGeneration: generation,
	})
	setCondition(status, metav1.Condition{
		Type:               inferencev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             inferencev1alpha1.ReasonInvalidSpec,
		Message:            msg,
		ObservedGeneration: generation,
	})
	status.Phase = inferencev1alpha1.PhaseDegraded
}

// setCondition wraps apimeta.SetStatusCondition, which handles the
// lastTransitionTime contract: the timestamp only moves when the status
// actually changes, so an unchanged condition does not churn on every
// reconcile.
func setCondition(status *inferencev1alpha1.ModelDeploymentStatus, cond metav1.Condition) {
	apimeta.SetStatusCondition(&status.Conditions, cond)
}

// findDeploymentCondition returns a condition from a Deployment's status, or
// nil when absent.
func findDeploymentCondition(dep *appsv1.Deployment, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == t {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// pluralReplicas renders a replica count for a human-readable message.
func pluralReplicas(n int32) string {
	if n == 1 {
		return "1 replica"
	}
	return strconv.Itoa(int(n)) + " replicas"
}

// endpointFor renders the published in-cluster endpoint.
func endpointFor(md *inferencev1alpha1.ModelDeployment) string {
	return naming.Endpoint(md.Name, md.Namespace)
}
