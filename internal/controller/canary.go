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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/canary"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/observability"
)

// rollout is the decision one reconcile pass reached about what should be
// running.
//
// It is deliberately a plain description of the desired end state rather than a
// list of operations. The controller then makes the cluster match it, which
// means the same struct describes a canary at 20%, a promotion, a rollback and
// a plain rolling update — and the apply path has no idea which of those it is
// implementing.
type rollout struct {
	// PrimaryRevision is the revision the primary Deployment must run. During a
	// canary this is the STABLE revision, not the target: the whole point is
	// that the old version keeps serving most of the traffic while the new one
	// is judged.
	PrimaryRevision string

	// CanaryRevision is the revision under evaluation, or "" when no canary
	// Deployment should exist.
	CanaryRevision string

	// PrimaryReplicas and CanaryReplicas sum to spec.replicas.
	PrimaryReplicas int32
	CanaryReplicas  int32

	// Output is the state machine's decision, when a canary strategy is in
	// play. Zero-valued otherwise.
	Output canary.Output

	// Round is this pass's analysis result, or nil if none ran.
	Round *canary.Round

	// Active reports whether a canary Deployment should exist.
	Active bool

	// RequeueAfter is when to look again.
	RequeueAfter time.Duration

	// RolledBack marks a pass that reverted a stalled non-canary rollout, so
	// status can say so.
	RolledBack bool
}

// planRollout decides what should be running this pass.
//
// The two strategies are handled by two different mechanisms and that is
// intentional. A RollingUpdate is delegated wholesale to the Deployment
// controller, which already implements it correctly; the only thing this
// operator adds is noticing that it STALLED and reverting, which a Deployment
// never does. A Canary is driven by the pure state machine in internal/canary,
// which this function feeds and whose decision it carries out.
func (r *ModelDeploymentReconciler) planRollout(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	target string,
	primaryDep *appsv1.Deployment,
	canaryDep *appsv1.Deployment,
) rollout {
	total := replicasFor(md)

	res := rollout{PrimaryRevision: target, PrimaryReplicas: total}
	if md.Spec.Rollout.IsCanary() {
		res = r.planCanary(ctx, md, target, canaryDep, total)
	}

	return r.guardAgainstStall(md, res, primaryDep)
}

// guardAgainstStall reverts a primary rollout that blew its progress deadline.
//
// # The one thing a Deployment genuinely cannot do
//
// Kubernetes implements rolling updates correctly and this operator delegates
// them wholesale rather than reimplementing them badly. What a Deployment does
// NOT do is act on its own failure: once it sets Progressing=False with reason
// ProgressDeadlineExceeded it stays there indefinitely, serving whatever mix of
// old and new pods it happened to reach, and nothing in the system ever
// revisits the decision. A bad image reference, a missing secret, a model file
// that will not parse — all of them leave a cluster in that state until a human
// notices.
//
// This is deliberately applied to BOTH strategies. A stalled RollingUpdate is
// the more common failure in practice, and a canary that has just been promoted
// is rolling the winning revision out through exactly the same mechanism, so it
// can stall in exactly the same way.
//
// It is skipped while a canary is actively serving: there the primary is
// running the old, known-good revision by construction, so a stall means
// something is wrong with the CLUSTER rather than with the release, and
// "reverting" to the revision already running would achieve nothing.
func (r *ModelDeploymentReconciler) guardAgainstStall(
	md *inferencev1alpha1.ModelDeployment,
	res rollout,
	primaryDep *appsv1.Deployment,
) rollout {
	if res.Active || res.RolledBack {
		return res
	}
	if !md.Spec.Rollout.AutoRollbackEnabled() || !deploymentStalled(primaryDep) {
		return res
	}

	lastGood := md.Status.LastGoodRevision
	if lastGood == "" || lastGood == res.PrimaryRevision {
		// Nothing to revert to, or the primary is ALREADY on the last
		// known-good — which is the state right after a rollback. Re-applying
		// it would be a no-op the first time and an event storm every time
		// after, since the stalled condition persists until the rollout
		// actually recovers.
		return res
	}

	failed := res.PrimaryRevision
	res.PrimaryRevision = lastGood
	res.CanaryRevision = ""
	res.CanaryReplicas = 0
	res.RolledBack = true

	// Record the rejection on the canary state too, so a canary strategy does
	// not immediately start evaluating the very revision that just failed to
	// roll out at all.
	res.Output.State.FailedRevision = failed

	return res
}

// planCanary drives the progressive-delivery state machine.
func (r *ModelDeploymentReconciler) planCanary(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	target string,
	canaryDep *appsv1.Deployment,
	total int32,
) rollout {
	log := logf.FromContext(ctx).WithName("canary")

	plan := canaryPlan(md)
	state := canaryStateFrom(md)
	now := r.now()

	in := canary.Input{
		Now:             now,
		Plan:            plan,
		State:           state,
		TargetRevision:  target,
		StableRevision:  md.Status.LastGoodRevision,
		TotalReplicas:   total,
		CanaryAvailable: canaryDep != nil && canaryDep.Status.ReadyReplicas > 0,
		Approved:        annotationTrue(md, naming.AnnoPromote),
		Aborted:         annotationTrue(md, naming.AnnoAbort),
	}

	// Analysis is run ONLY when the state machine says a round is due. The
	// alternative — querying on every reconcile — would put Prometheus in the
	// path of every watch event, several times a second under an active
	// rollout, and would make the analysis interval a property of how noisy the
	// cluster happens to be rather than of the spec.
	if canary.DueForAnalysis(now, plan, state) {
		round := r.analyse(ctx, md, plan)
		in.Round = &round
		log.V(1).Info("analysis round", "verdict", round.Verdict, "checks", len(round.Checks))
	}

	out := canary.Next(in)

	res := rollout{
		Output:          out,
		Round:           in.Round,
		PrimaryReplicas: out.Primary,
		CanaryReplicas:  out.Canary,
		RequeueAfter:    out.RequeueAfter,
	}

	switch out.Action {
	case canary.ActionPromote:
		// The canary's revision becomes the primary's. The canary Deployment is
		// removed, not renamed: names are symmetric from day one precisely so
		// that a promotion never has to rename anything, and the primary simply
		// converges onto the new pod template through its own rolling update.
		res.PrimaryRevision = out.State.Revision
		res.PrimaryReplicas = total
		res.CanaryReplicas = 0

	case canary.ActionRollback:
		res.PrimaryRevision = firstNonEmpty(out.State.StableRevision, md.Status.LastGoodRevision, target)
		res.PrimaryReplicas = total
		res.CanaryReplicas = 0

	case canary.ActionNone:
		// No canary in flight: either the target is already stable, or it was
		// rolled back and will not be retried, or there is nothing to fall back
		// to yet. In the last case the revision must still be rolled out —
		// otherwise a first-ever deployment configured for canary would never
		// start at all.
		res.PrimaryRevision = target
		res.PrimaryReplicas = total
		res.CanaryReplicas = 0

	default:
		// Start, Advance, Wait, Pause: the canary runs the target revision
		// while the primary keeps serving the stable one.
		res.PrimaryRevision = firstNonEmpty(md.Status.LastGoodRevision, target)
		res.CanaryRevision = out.State.Revision
		res.Active = res.CanaryReplicas > 0
	}

	return res
}

// analyse runs one analysis round.
//
// A missing provider address is reported as an Error verdict rather than as a
// reconcile failure. The distinction matters: an Error spends the error budget,
// which resets on the first successful round and never rolls anything back,
// whereas failing the reconcile would retry with backoff and leave the canary's
// status silent about why nothing is happening.
func (r *ModelDeploymentReconciler) analyse(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
	plan canary.Plan,
) canary.Round {
	spec := &md.Spec.Rollout.Canary.Analysis
	metrics := spec.ResolvedMetrics()

	provider := r.providerFor(md)
	if provider == nil {
		checks := make([]inferencev1alpha1.MetricCheck, 0, len(metrics))
		for i := range metrics {
			checks = append(checks, analysis.Errored(metrics[i],
				"no metric provider is configured: set spec.rollout.canary.analysis.provider.address"))
		}
		return canary.Aggregate(checks)
	}

	analyzer := &analysis.Analyzer{Provider: provider}

	started := r.now()
	checks := analyzer.Run(ctx, analysis.Request{
		Model:          md.Spec.Model.Name,
		Window:         analysis.PromDuration(spec.ResolvedWindow().Duration),
		MinRequestRate: spec.ResolvedMinRequestRate(),
		Metrics:        metrics,
	})

	round := canary.Aggregate(checks)

	// Timed with the INJECTED clock, like everything else here. Under a fake
	// clock that yields a duration of zero, which is honest — the round did
	// take no simulated time — and it keeps the lint rule banning time.Now()
	// outside main intact.
	observability.RecordAnalysisRound(resourceKeyFor(md), string(round.Verdict), r.now().Sub(started))

	_ = plan
	return round
}

// resourceKeyFor identifies a ModelDeployment's operator-metric series.
func resourceKeyFor(md *inferencev1alpha1.ModelDeployment) observability.ResourceKey {
	return observability.ResourceKey{
		Model:     md.Spec.Model.Name,
		Namespace: md.Namespace,
		Name:      md.Name,
	}
}

// providerFor builds the metric provider for a ModelDeployment.
//
// An injected Provider takes precedence over the spec, which is what lets an
// envtest suite drive a scripted sequence of verdicts — "pass, pass, fail,
// fail" — and assert that a rollback fires on exactly the second failure. That
// assertion is impossible against a live Prometheus, and it is the single most
// valuable test in this package.
func (r *ModelDeploymentReconciler) providerFor(md *inferencev1alpha1.ModelDeployment) analysis.Provider {
	if r.Provider != nil {
		return r.Provider
	}
	spec := &md.Spec.Rollout.Canary.Analysis
	if spec.Provider.Address == "" {
		return nil
	}
	return &analysis.PrometheusProvider{
		Address: spec.Provider.Address,
		Timeout: spec.ResolvedTimeout().Duration,
	}
}

// canaryPlan resolves the spec into the state machine's flat Plan.
func canaryPlan(md *inferencev1alpha1.ModelDeployment) canary.Plan {
	c := md.Spec.Rollout.Canary
	a := &c.Analysis

	plan := canary.Plan{
		Weights:               c.StepLadder(),
		Interval:              a.ResolvedInterval().Duration,
		InitialDelay:          a.ResolvedInitialDelay().Duration,
		FailureThreshold:      a.ResolvedFailureThreshold(),
		ConsecutiveErrorLimit: a.ResolvedConsecutiveErrorLimit(),
		InconclusiveLimit:     a.ResolvedInconclusiveLimit(),
		OnInconclusive:        a.ResolvedOnInconclusive(),
		RequireApproval:       c.ApprovalRequired(),
	}

	if c.Scale != nil && c.Scale.Replicas != nil {
		plan.CanaryReplicaOverride = c.Scale.Replicas
	}
	return plan
}

// canaryStateFrom rebuilds the state machine's position from status.
//
// Status is the only place this state lives. Keeping it there rather than in
// memory is what makes the controller restartable mid-rollout: a leader
// election handover, a crash, or a rolling upgrade of the operator itself all
// resume the canary at the step it had reached, with its failure count intact.
// An in-memory cache would silently reset the budget on every restart, which is
// how a broken canary gets promoted by an operator that was merely rescheduled.
func canaryStateFrom(md *inferencev1alpha1.ModelDeployment) canary.State {
	cs := md.Status.Canary
	if cs == nil {
		return canary.State{}
	}

	st := canary.State{
		Phase:          canaryPhaseFrom(md.Status.Phase),
		Revision:       cs.Revision,
		StableRevision: cs.StableRevision,
		FailedRevision: cs.FailedRevision,
		Step:           cs.Step,
	}
	st.SetCounters(cs.FailedChecks, cs.ConsecutiveErrors, cs.ConsecutiveInconclusive)

	if cs.StartTime != nil {
		st.StartedAt = cs.StartTime.Time
	}
	if cs.LastAnalysisTime != nil {
		st.LastAnalysis = cs.LastAnalysisTime.Time
	}
	if cs.AvailableSince != nil {
		st.AvailableSince = cs.AvailableSince.Time
	}
	return st
}

// canaryPhaseFrom maps the API's coarse phase back to the state machine's.
func canaryPhaseFrom(p inferencev1alpha1.Phase) canary.Phase {
	switch p {
	case inferencev1alpha1.PhaseCanarying:
		return canary.PhaseProgressing
	case inferencev1alpha1.PhasePaused:
		return canary.PhasePaused
	case inferencev1alpha1.PhasePromoting:
		return canary.PhasePromoting
	case inferencev1alpha1.PhaseRollingBack:
		return canary.PhaseRollingBack
	default:
		return canary.PhaseIdle
	}
}

// canaryStatusFrom renders the state machine's output for status.
func canaryStatusFrom(out canary.Output, round *canary.Round, prev *inferencev1alpha1.CanaryStatus) *inferencev1alpha1.CanaryStatus {
	st := out.State

	if st.Phase == canary.PhaseIdle && st.Revision == "" && st.FailedRevision == "" {
		// Never ran, or fully settled with nothing to remember. Returning nil
		// keeps the status clean rather than leaving a husk of zero fields that
		// reads like a canary that failed silently.
		return nil
	}

	cs := &inferencev1alpha1.CanaryStatus{
		Revision:                st.Revision,
		StableRevision:          st.StableRevision,
		FailedRevision:          st.FailedRevision,
		DesiredWeight:           out.DesiredWeight,
		CurrentWeight:           out.RealizedWeight,
		Step:                    st.Step,
		FailedChecks:            st.FailedChecks(),
		ConsecutiveErrors:       st.ConsecutiveErrors(),
		ConsecutiveInconclusive: st.ConsecutiveInconclusive(),
		Message:                 out.Message,
	}

	if !st.StartedAt.IsZero() {
		cs.StartTime = &metav1.Time{Time: st.StartedAt}
	}
	if !st.LastAnalysis.IsZero() {
		cs.LastAnalysisTime = &metav1.Time{Time: st.LastAnalysis}
	}
	if !st.AvailableSince.IsZero() {
		cs.AvailableSince = &metav1.Time{Time: st.AvailableSince}
	}

	switch {
	case round != nil:
		cs.Checks = round.Checks
	case prev != nil:
		// Carry the previous round forward. Between rounds the checks would
		// otherwise vanish and reappear, which makes `kubectl get -w` look like
		// the analysis is being reset every few seconds.
		cs.Checks = prev.Checks
	}

	return cs
}

// canaryDeploymentFor reads the canary Deployment, tolerating its absence.
func (r *ModelDeploymentReconciler) canaryDeploymentFor(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: md.Namespace, Name: naming.CanaryDeployment(md.Name)}

	switch err := r.Get(ctx, key, &dep); {
	case err == nil:
		return &dep, nil
	case apierrors.IsNotFound(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("reading canary Deployment: %w", err)
	}
}

// deleteCanaryDeployment removes the canary variant.
//
// Deleting rather than scaling to zero. A Deployment left at zero replicas
// keeps its ReplicaSets, keeps matching the metrics Service's selector, and —
// the part that actually bites — keeps the llmcp_shim_info series alive with
// variant="canary" for the whole of Prometheus's staleness window, so the
// canary dashboard shows a variant that no longer exists.
func (r *ModelDeploymentReconciler) deleteCanaryDeployment(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: md.Namespace,
			Name:      naming.CanaryDeployment(md.Name),
		},
	}
	if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting canary Deployment: %w", err)
	}
	return nil
}

// clearRolloutAnnotations removes promote/abort once they have been acted on.
//
// A promote annotation left in place would auto-approve the NEXT canary before
// anyone had looked at it — the operator's earlier "yes" silently applying to a
// release they have not seen. Clearing it makes each approval mean exactly one
// promotion.
func (r *ModelDeploymentReconciler) clearRolloutAnnotations(
	ctx context.Context,
	md *inferencev1alpha1.ModelDeployment,
) error {
	if !annotationTrue(md, naming.AnnoPromote) && !annotationTrue(md, naming.AnnoAbort) {
		return nil
	}

	// A merge patch setting the keys to null, rather than a read-modify-write.
	// The alternative would race with anything else editing the object — an
	// operator adding a second annotation in the same second, most obviously —
	// and would lose their edit. A patch touches only these two keys.
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{%q:null,%q:null}}}`, naming.AnnoPromote, naming.AnnoAbort)

	if err := r.Patch(ctx, md, client.RawPatch(types.MergePatchType, patch)); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("clearing rollout annotations: %w", err)
	}
	return nil
}

// emitRolloutEvents records the audit trail for one canary decision.
//
// These events are not decoration. The canary dashboard sources its annotations
// from them, and the annotated rollback moment — a vertical line on the latency
// graph at the instant the controller decided — is the single most useful thing
// on it. They are also what `kubectl get events --field-selector
// reason=CanaryRolledBack` finds, which is how the demo proves the rollback was
// automatic rather than staged.
func (r *ModelDeploymentReconciler) emitRolloutEvents(
	md *inferencev1alpha1.ModelDeployment,
	res rollout,
) {
	// md still carries the PREVIOUS status: this runs before the status write.
	// Every emission below is gated on an actual transition, because a
	// reconcile may run many times between two real changes — a watch event on
	// an unrelated field, a resync, a status write of our own — and an event
	// per reconcile would bury the three that matter under hundreds that do
	// not. The canary dashboard sources its annotations from these, so a
	// duplicate is not merely noise, it is a second vertical line on the graph
	// at a moment when nothing happened.
	prevPhase := md.Status.Phase
	prevCanary := md.Status.Canary

	if res.RolledBack {
		if prevPhase != inferencev1alpha1.PhaseDegraded {
			r.event(md, corev1.EventTypeWarning, inferencev1alpha1.EventReasonRolloutStalled, "Rollback",
				"Rollout exceeded its progress deadline; reverting to revision "+res.PrimaryRevision)
		}
		return
	}

	out := res.Output

	// The gauges are refreshed on EVERY pass with a canary in flight, not only
	// on a transition, so the weight graph steps instead of being a row of
	// disconnected points. The counters below are gated on a transition for the
	// opposite reason: a counter incremented per reconcile would count the same
	// promotion hundreds of times.
	key := resourceKeyFor(md)
	if res.Active {
		observability.RecordCanaryState(key,
			out.DesiredWeight, out.RealizedWeight, out.State.FailedChecks())
	} else {
		// A gauge left behind would report a canary permanently carrying 50% of
		// traffic long after it was promoted.
		observability.ForgetCanary(key)
	}

	switch out.Action {
	case canary.ActionStart:
		if prevCanary == nil || prevCanary.Revision != out.State.Revision {
			r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonCanaryStarted, "Canary", out.Message)
		}
	case canary.ActionAdvance:
		if prevCanary == nil || prevCanary.Step != out.State.Step {
			r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonCanaryAdvanced, "Canary", out.Message)
		}
	case canary.ActionPromote:
		if prevPhase != inferencev1alpha1.PhasePromoting {
			r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonCanaryPromoted, "Promote", out.Message)
			observability.RecordCanaryPromotion(key)
		}
	case canary.ActionPause:
		if prevPhase != inferencev1alpha1.PhasePaused {
			r.event(md, corev1.EventTypeNormal, inferencev1alpha1.EventReasonCanaryPaused, "Pause", out.Message)
		}
	case canary.ActionRollback:
		if prevPhase != inferencev1alpha1.PhaseRollingBack {
			reason := inferencev1alpha1.EventReasonCanaryRolledBack
			if out.Reason == inferencev1alpha1.ReasonCanaryAborted {
				reason = inferencev1alpha1.EventReasonCanaryAborted
			}
			r.event(md, corev1.EventTypeWarning, reason, "Rollback", out.Message)
			// Keyed by CAUSE. Collapsing an analysis-driven rollback together
			// with an operator abort and a provider-outage abort would make
			// "the gate is working" and "the monitoring broke" look identical
			// on a graph — the same conflation the four-valued verdicts exist
			// to prevent.
			observability.RecordCanaryRollback(key, out.Reason)
		}
	}

	// A failed round that did not (yet) roll back still deserves an event: it
	// is the evidence that the gate is working, and without it a rollback looks
	// like it came out of nowhere.
	if res.Round != nil && res.Round.Verdict == inferencev1alpha1.VerdictFail &&
		out.Action != canary.ActionRollback {
		r.event(md, corev1.EventTypeWarning, inferencev1alpha1.EventReasonAnalysisFailed, "Analyze",
			out.Message+" — "+failedCheckSummary(res.Round))
	}

	if canary.IsQuantized(out.DesiredWeight, out.RealizedWeight) && res.Active {
		r.event(md, corev1.EventTypeWarning, inferencev1alpha1.EventReasonWeightQuantized, "Split",
			fmt.Sprintf(
				"Requested %d%% traffic to the canary but %d replicas can only express %d%%; "+
					"increase spec.replicas for a finer split",
				out.DesiredWeight, out.Primary+out.Canary, out.RealizedWeight))
	}
}

// failedCheckSummary names which metrics failed, for the event message.
func failedCheckSummary(round *canary.Round) string {
	for _, c := range round.Checks {
		if c.Verdict == inferencev1alpha1.VerdictFail {
			return c.Name + ": " + c.Message
		}
	}
	return "no failing check was recorded"
}

// deploymentStalled reports whether the Deployment controller declared the
// rollout failed.
//
// Read from the child rather than recomputed. The Deployment controller already
// tracks progress deadlines correctly, and a second implementation here would
// be one that can disagree with it — producing a rollback the Deployment still
// considers healthy, or none when it does not.
func deploymentStalled(dep *appsv1.Deployment) bool {
	if dep == nil {
		return false
	}
	cond := findDeploymentCondition(dep, appsv1.DeploymentProgressing)
	return cond != nil &&
		cond.Status == corev1.ConditionFalse &&
		cond.Reason == inferencev1alpha1.ReasonProgressDeadlineExceeded
}

// annotationEnabled is the value an operator writes to arm promote or abort.
//
// Exactly "true", and nothing else. Accepting "yes", "1" or "" would make the
// most consequential control in the API forgiving in a direction where being
// forgiving is wrong: an accidental annotation should do nothing, not promote a
// release.
const annotationEnabled = "true"

// annotationTrue reports whether an annotation is armed.
func annotationTrue(md *inferencev1alpha1.ModelDeployment, key string) bool {
	return md.Annotations[key] == annotationEnabled
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
