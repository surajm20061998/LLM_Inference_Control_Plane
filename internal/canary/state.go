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

// Package canary is the rollout state machine, and it is deliberately the only
// part of this operator that contains no I/O at all.
//
// # Why Next is pure
//
// Next takes a value and returns a value. No client, no context, no
// time.Now(), no logger. Every input it needs — the clock reading, the analysis
// outcome, whether the canary's pods are ready — arrives as a struct field, and
// every effect it wants — advance, promote, roll back, requeue in 30 seconds —
// leaves as a struct field for the caller to carry out.
//
// This is not stylistic. Rollout logic is where the expensive bugs live, and
// it is also the part hardest to exercise against a real cluster: reproducing
// "the fourth analysis round failed while the third step was still scaling up"
// takes minutes of wall time and a lot of luck. As a pure function the same
// scenario is four lines in a table test that runs in microseconds, and an
// entire five-step canary — including its timing — can be replayed
// deterministically by stepping a fake clock.
//
// The specific consequence worth stating: RequeueAfter is a RETURN VALUE, not a
// side effect. A test asserts on it directly instead of measuring how long the
// controller slept.
package canary

import (
	"time"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Phase is where a canary is in its lifecycle.
type Phase string

const (
	// PhaseIdle means no canary is in flight.
	PhaseIdle Phase = ""

	// PhaseWaiting means the canary's pods are not serving yet, or the
	// warm-up delay has not elapsed. No analysis runs in this phase.
	PhaseWaiting Phase = "Waiting"

	// PhaseProgressing means the canary is being analysed and its weight is
	// climbing.
	PhaseProgressing Phase = "Progressing"

	// PhasePaused means the canary passed every check and is waiting for an
	// operator to approve promotion. Nothing leaves this phase without
	// external input.
	PhasePaused Phase = "Paused"

	// PhasePromoting means the canary won and its revision is becoming the
	// primary.
	PhasePromoting Phase = "Promoting"

	// PhaseRollingBack means the canary lost and is being torn down.
	PhaseRollingBack Phase = "RollingBack"
)

// Action is what the caller must actually do.
//
// The state machine never performs an action; it names one. That separation is
// what lets the same decision be asserted in a unit test and executed against a
// cluster without either one being a mock of the other.
type Action string

const (
	// ActionNone means nothing needs to change.
	ActionNone Action = "None"

	// ActionStart means create the canary Deployment at the first weight.
	ActionStart Action = "Start"

	// ActionWait means hold the current split and check again later.
	ActionWait Action = "Wait"

	// ActionAdvance means move to the next rung of the weight ladder.
	ActionAdvance Action = "Advance"

	// ActionPromote means make the canary revision the primary and remove the
	// canary Deployment.
	ActionPromote Action = "Promote"

	// ActionRollback means restore the stable revision and remove the canary
	// Deployment.
	ActionRollback Action = "Rollback"

	// ActionPause means hold and wait for a human.
	ActionPause Action = "Pause"
)

// Plan is the resolved, fully-defaulted canary configuration.
//
// It is a flat value rather than a pointer to the API type so that Next cannot
// accidentally depend on anything the API type gains later, and so that a test
// can construct one without building a whole ModelDeployment.
type Plan struct {
	// Weights is the strictly increasing ladder, e.g. [20, 40, 50]. An empty
	// ladder means promote immediately, which is a valid degenerate case.
	Weights []int32

	// Interval is how often an analysis round runs.
	Interval time.Duration

	// InitialDelay is the warm-up grace after the canary becomes available,
	// before the first round.
	InitialDelay time.Duration

	// FailureThreshold, ConsecutiveErrorLimit and InconclusiveLimit are the
	// three disjoint budgets. See counters in verdict.go.
	FailureThreshold      int32
	ConsecutiveErrorLimit int32
	InconclusiveLimit     int32

	// OnInconclusive is what to do once InconclusiveLimit is reached.
	OnInconclusive inferencev1alpha1.InconclusiveAction

	// RequireApproval holds before the final promotion.
	RequireApproval bool

	// CanaryReplicaOverride pins the canary's pod count independently of its
	// weight. Nil means size it proportionally.
	CanaryReplicaOverride *int32
}

// State is the canary's persisted position. It round-trips through
// status.canary, which is why every field is a plain value.
type State struct {
	Phase Phase

	// Revision is the revision under evaluation.
	Revision string

	// StableRevision is what a rollback restores.
	StableRevision string

	// FailedRevision is the revision a previous rollback rejected. A canary is
	// not restarted for it; see the field's doc on the API type.
	FailedRevision string

	// Step is the zero-based index into Plan.Weights.
	Step int32

	// AvailableSince is when the canary's pods first became ready. The warm-up
	// delay is measured from here, not from the canary's start, because a
	// canary that spent four minutes pulling a model image has not been warm
	// for four minutes.
	AvailableSince time.Time

	// LastAnalysis is when the previous round ran. Zero means none has.
	LastAnalysis time.Time

	// StartedAt is when this canary began.
	StartedAt time.Time

	counters
}

// Input is everything Next needs. Every field is supplied by the caller; Next
// reads nothing else.
type Input struct {
	// Now is the current time. Injected, never read from the clock, so an
	// entire multi-step rollout can be replayed in microseconds.
	Now time.Time

	// Plan is the resolved configuration.
	Plan Plan

	// State is the position persisted from the previous pass.
	State State

	// TargetRevision is the revision the spec currently describes.
	TargetRevision string

	// StableRevision is the last revision that reached availability — the
	// rollback target. An empty value means a canary cannot start: a rollout
	// with nothing to fall back to is not a canary.
	StableRevision string

	// TotalReplicas is spec.replicas: the total across both variants.
	TotalReplicas int32

	// CanaryAvailable reports whether the canary Deployment has at least one
	// ready pod.
	CanaryAvailable bool

	// Round is this pass's analysis outcome, or nil when no round was run —
	// either because it was not due, or because the canary is not being
	// analysed yet.
	Round *Round

	// Approved and Aborted are the operator's out-of-band signals, read from
	// annotations by the caller.
	Approved bool
	Aborted  bool
}

// Output is the decision. Nothing here has happened yet.
type Output struct {
	// State is the position to persist.
	State State

	// Action is what the caller must do.
	Action Action

	// DesiredWeight is the traffic share the current step asks for.
	DesiredWeight int32

	// Primary and Canary are the replica counts implementing DesiredWeight.
	Primary int32
	Canary  int32

	// RealizedWeight is the share those counts actually achieve. It differs
	// from DesiredWeight whenever the replica count cannot express it.
	RealizedWeight int32

	// RequeueAfter is when the caller should come back. Zero means "no timer
	// needed" — a watch will wake the controller when something changes.
	//
	// A return value rather than a side effect: this is what makes the state
	// machine's timing assertable without a test ever sleeping.
	RequeueAfter time.Duration

	// Reason is a machine-readable explanation, suitable for a condition.
	Reason string

	// Message is the human-readable form.
	Message string
}

// DueForAnalysis reports whether an analysis round should run this pass.
//
// It is separate from Next, and pure, because of an ordering problem: the
// caller must know whether to spend a query on Prometheus BEFORE it can hand
// Next a Round. Folding the decision into Next would mean either querying on
// every reconcile — several times a second under a busy watch — or letting the
// controller invent its own schedule, which would put the rollout's timing
// somewhere other than the state machine that is supposed to own it.
//
// Three gates, in order:
//
//  1. The canary must be in a phase that is actually being analysed.
//  2. Its pods must be serving. Analysing a canary with no ready pods measures
//     the absence of traffic and reports Inconclusive forever.
//  3. The warm-up delay must have elapsed since the pods became ready. A
//     freshly started inference pod has a cold KV cache and is still faulting
//     the model's pages in; its first seconds of TTFT are genuinely terrible
//     and genuinely unrepresentative, and measuring them would fail every
//     canary that was ever going to be fine.
func DueForAnalysis(now time.Time, plan Plan, state State) bool {
	if state.Phase != PhaseProgressing && state.Phase != PhaseWaiting {
		return false
	}
	if state.AvailableSince.IsZero() {
		return false
	}
	if now.Before(state.AvailableSince.Add(plan.InitialDelay)) {
		return false
	}
	if state.LastAnalysis.IsZero() {
		return true
	}
	return !now.Before(state.LastAnalysis.Add(plan.Interval))
}

// Next computes the canary's next position.
//
// It is a pure function of Input. Calling it twice with the same input yields
// the same output, and calling it with an input it has already produced is a
// no-op — the idempotency property the caller relies on, since a reconcile may
// run any number of times between two actual changes.
func Next(in Input) Output {
	st := in.State

	// --- Terminal and external signals, checked before anything else. -------

	// Abort outranks every other consideration, including an approval that
	// arrived in the same pass. An operator hitting the brakes must not race
	// with the machine's own optimism.
	if in.Aborted && isActive(st.Phase) {
		return rollback(in, st, inferencev1alpha1.ReasonCanaryAborted,
			"Rollout aborted by "+annoAbort)
	}

	// A spec change mid-flight restarts the canary from step zero, and
	// deliberately does not try to be clever about it. The alternative —
	// carrying the accumulated failure count and the current weight across to a
	// revision that has never been measured — would mean promoting a new build
	// on the strength of checks run against a different one. Restarting costs
	// time; the clever version costs correctness.
	if isActive(st.Phase) && st.Revision != in.TargetRevision {
		return start(in, inferencev1alpha1.ReasonCanaryProgressing,
			"Spec changed mid-rollout; restarting the canary at step 0 for revision "+in.TargetRevision)
	}

	switch st.Phase {
	case PhasePromoting:
		return promoting(in, st)
	case PhaseIdle, PhaseRollingBack:
		return begin(in)
	case PhasePaused:
		return paused(in, st)
	case PhaseWaiting, PhaseProgressing:
		return analyse(in, st)
	default:
		// An unknown phase means a status written by a different version of
		// this controller. Restarting is the only safe interpretation: it
		// cannot be reasoned about, and continuing from it would be guessing.
		return begin(in)
	}
}

// annoAbort names the abort annotation in operator-facing messages. It is
// duplicated from internal/naming rather than imported so that this package
// stays free of dependencies on anything cluster-shaped.
const (
	annoAbort   = "llmcp.io/abort"
	annoPromote = "llmcp.io/promote"
)

// begin decides whether a canary should start at all.
func begin(in Input) Output {
	// A revision a previous rollback rejected is not retried. The spec still
	// names it, so target and stable still differ, and without this guard the
	// machine would start an identical canary immediately — an infinite loop of
	// failing rollouts, each costing a full analysis window and a Deployment
	// churn. A rollback sticks until a human changes the spec.
	if in.TargetRevision != "" && in.TargetRevision == in.State.FailedRevision {
		return Output{
			State:          State{Phase: PhaseIdle, FailedRevision: in.State.FailedRevision},
			Action:         ActionNone,
			Primary:        in.TotalReplicas,
			DesiredWeight:  0,
			RealizedWeight: 0,
			Reason:         inferencev1alpha1.ReasonCanaryRolledBack,
			Message: "Revision " + in.TargetRevision + " was rolled back and will not be retried; " +
				"change the spec to try again",
		}
	}

	// Nothing to do: the target revision is already what is stable.
	if in.TargetRevision == in.StableRevision || in.TargetRevision == "" {
		return Output{
			State:          State{Phase: PhaseIdle, FailedRevision: in.State.FailedRevision},
			Action:         ActionNone,
			Primary:        in.TotalReplicas,
			DesiredWeight:  0,
			RealizedWeight: 0,
			Reason:         inferencev1alpha1.ReasonCanaryNotRunning,
			Message:        "No canary in flight",
		}
	}

	// A canary with no rollback target is not a canary. This is the state on a
	// first-ever deployment, and the right answer is to roll the revision out
	// normally rather than to run an unfalsifiable comparison against nothing.
	if in.StableRevision == "" {
		return Output{
			State:          State{Phase: PhaseIdle, FailedRevision: in.State.FailedRevision},
			Action:         ActionNone,
			Primary:        in.TotalReplicas,
			DesiredWeight:  0,
			RealizedWeight: 0,
			Reason:         inferencev1alpha1.ReasonNoLastGoodRevision,
			Message: "No revision has reached availability yet, so there is nothing to roll back to; " +
				"rolling out directly instead of canarying",
		}
	}

	// A ladder with no rungs means the user asked for a canary with no steps.
	// Promote rather than sit still: an empty ladder is a configuration
	// mistake, and stalling on it forever is a worse answer than doing the
	// thing it most plausibly meant.
	if len(in.Plan.Weights) == 0 {
		return Output{
			State: State{
				Phase:          PhasePromoting,
				Revision:       in.TargetRevision,
				StableRevision: in.StableRevision,
				FailedRevision: in.State.FailedRevision,
				StartedAt:      in.Now,
			},
			Action:         ActionPromote,
			Primary:        in.TotalReplicas,
			DesiredWeight:  100,
			RealizedWeight: 100,
			Reason:         inferencev1alpha1.ReasonCanaryPromoted,
			Message:        "The weight ladder is empty; promoting revision " + in.TargetRevision + " directly",
		}
	}

	// Replica-based traffic splitting needs at least two pods, and at a total of
	// one the primary keeps it — see Split. Detecting that HERE, before
	// starting, is what turns a silent stall into an explanation: without this
	// the machine would create a canary Deployment with zero replicas, wait
	// forever for it to become available, and report "waiting for canary
	// replicas" with no hint that it never can.
	if _, c := splitFor(in, weightAt(in.Plan, 0)); c == 0 {
		return Output{
			State:          State{Phase: PhaseIdle, FailedRevision: in.State.FailedRevision},
			Action:         ActionNone,
			Primary:        in.TotalReplicas,
			DesiredWeight:  0,
			RealizedWeight: 0,
			Reason:         inferencev1alpha1.ReasonInsufficientReplicas,
			Message: "Replica-based traffic splitting needs at least 2 replicas to divide; " +
				"spec.replicas is " + itoa(in.TotalReplicas) + ", so revision " + in.TargetRevision +
				" is being rolled out directly without analysis",
		}
	}

	return start(in, inferencev1alpha1.ReasonCanaryProgressing,
		"Canary started for revision "+in.TargetRevision)
}

// start creates a fresh canary at step zero.
func start(in Input, reason, message string) Output {
	st := State{
		Phase:          PhaseWaiting,
		Revision:       in.TargetRevision,
		StableRevision: in.StableRevision,
		FailedRevision: in.State.FailedRevision,
		Step:           0,
		StartedAt:      in.Now,
	}

	primary, canary := splitFor(in, weightAt(in.Plan, 0))
	return Output{
		State:          st,
		Action:         ActionStart,
		DesiredWeight:  weightAt(in.Plan, 0),
		Primary:        primary,
		Canary:         canary,
		RealizedWeight: RealizedWeight(primary, canary),
		// Requeue on the analysis interval even though the canary's pods are
		// watched. The watch fires when the Deployment's status changes, which
		// covers becoming available — but the warm-up delay expiring is not an
		// event anything emits, and without a timer the first analysis round
		// would wait for an unrelated change to wake the controller.
		RequeueAfter: in.Plan.Interval,
		Reason:       reason,
		Message:      message,
	}
}

// promoting holds the promotion until the primary has actually converged.
//
// # Why this is a phase and not an instant
//
// Promotion is not atomic. The canary Deployment is removed and the PRIMARY is
// re-pointed at the winning revision, which then has to roll out through its
// own rolling update — seconds for a stub, minutes for a real model. Until that
// finishes, the last known-good revision is still the OLD one.
//
// Without this phase the very next reconcile would see a target revision that
// differs from the stable one, conclude that a rollout is needed, and start a
// brand-new canary for the revision it just promoted. The result is an infinite
// loop of canaries, each one promoting into the next, and it is entirely
// invisible until someone notices the Deployment never settles.
//
// So the machine stays here, re-asserting the same desired state — which is a
// no-op for the apply — until StableRevision catches up.
func promoting(in Input, st State) Output {
	// The spec moved while the promotion was in flight. The revision being
	// promoted is no longer the one anyone wants, so start over rather than
	// finish delivering something already superseded.
	if st.Revision != in.TargetRevision {
		return begin(in)
	}

	if in.StableRevision == st.Revision {
		return Output{
			State:          State{Phase: PhaseIdle, FailedRevision: st.FailedRevision},
			Action:         ActionNone,
			Primary:        in.TotalReplicas,
			DesiredWeight:  0,
			RealizedWeight: 0,
			Reason:         inferencev1alpha1.ReasonCanaryPromoted,
			Message:        "Revision " + st.Revision + " is promoted and serving all traffic",
		}
	}

	return Output{
		State:          st,
		Action:         ActionPromote,
		DesiredWeight:  100,
		Primary:        in.TotalReplicas,
		RealizedWeight: 100,
		Reason:         inferencev1alpha1.ReasonCanaryPromoted,
		Message:        "Promoting revision " + st.Revision + "; waiting for the primary to converge",
	}
}

// paused holds until an operator approves or aborts.
//
// Nothing else moves this state. That is the contract of a manual gate: if the
// machine could talk itself out of a pause — on a timeout, say — the gate would
// be advisory, and an operator who stepped away for lunch would come back to a
// promoted release they never approved.
func paused(in Input, st State) Output {
	primary, canary := splitFor(in, currentWeight(in.Plan, st))

	if in.Approved {
		st.Phase = PhasePromoting
		return Output{
			State:          st,
			Action:         ActionPromote,
			DesiredWeight:  100,
			Primary:        in.TotalReplicas,
			RealizedWeight: 100,
			Reason:         inferencev1alpha1.ReasonCanaryPromoted,
			Message:        "Promotion approved via " + annoPromote,
		}
	}

	return Output{
		State:          st,
		Action:         ActionPause,
		DesiredWeight:  currentWeight(in.Plan, st),
		Primary:        primary,
		Canary:         canary,
		RealizedWeight: RealizedWeight(primary, canary),
		// No RequeueAfter. Approval arrives as an annotation change, which the
		// controller watches; a timer here would wake it every interval for as
		// long as the pause lasts, doing nothing each time.
		Reason:  inferencev1alpha1.ReasonAwaitingApproval,
		Message: "Waiting for approval: set the " + annoPromote + " annotation to \"true\" to promote",
	}
}

// analyse advances a canary that is already running.
func analyse(in Input, st State) Output {
	weight := currentWeight(in.Plan, st)
	primary, canary := splitFor(in, weight)

	hold := func(phase Phase, reason, message string, requeue time.Duration) Output {
		st.Phase = phase
		return Output{
			State:          st,
			Action:         ActionWait,
			DesiredWeight:  weight,
			Primary:        primary,
			Canary:         canary,
			RealizedWeight: RealizedWeight(primary, canary),
			RequeueAfter:   requeue,
			Reason:         reason,
			Message:        message,
		}
	}

	// The canary's pods are not serving yet. Record when they become available
	// so the warm-up delay is measured from the right instant, and do not
	// analyse: a canary with no ready pods produces no traffic, and every
	// metric over it would be Inconclusive — burning the inconclusive budget on
	// a condition that is entirely expected.
	if !in.CanaryAvailable {
		st.AvailableSince = time.Time{}
		return hold(PhaseWaiting, inferencev1alpha1.ReasonCanaryProgressing,
			"Waiting for canary replicas to become available", in.Plan.Interval)
	}

	if st.AvailableSince.IsZero() {
		st.AvailableSince = in.Now
	}

	warmUntil := st.AvailableSince.Add(in.Plan.InitialDelay)
	if in.Now.Before(warmUntil) {
		return hold(PhaseProgressing, inferencev1alpha1.ReasonCanaryProgressing,
			"Warming up before the first analysis round", warmUntil.Sub(in.Now))
	}

	// No round was supplied: the caller decided it was not due. Come back when
	// it is.
	if in.Round == nil {
		return hold(PhaseProgressing, inferencev1alpha1.ReasonCanaryProgressing,
			"Canary is progressing", nextAnalysisIn(in, st))
	}

	st.LastAnalysis = in.Now
	st.counters = st.apply(in.Round.Verdict)

	switch in.Round.Verdict {
	case inferencev1alpha1.VerdictError:
		if st.consecErr >= in.Plan.ConsecutiveErrorLimit {
			// The provider has been unreachable for too long to keep a rollout
			// in flight, so the canary is torn down — but note this is an
			// ABORT, not a verdict on the release. It restores the stable
			// revision without ever claiming the canary was bad, and the
			// condition reason says AnalysisError rather than ChecksFailed so
			// the distinction survives into the audit trail.
			return rollback(in, st, inferencev1alpha1.ReasonAnalysisError,
				"Aborting: the metric provider has been unreachable for "+
					itoa(st.consecErr)+" consecutive rounds")
		}
		return hold(PhaseProgressing, inferencev1alpha1.ReasonAnalysisError,
			"Metric provider error ("+itoa(st.consecErr)+"/"+itoa(in.Plan.ConsecutiveErrorLimit)+
				"); holding at the current weight rather than rolling back", in.Plan.Interval)

	case inferencev1alpha1.VerdictInconclusive:
		if st.consecInconcl >= in.Plan.InconclusiveLimit {
			return onInconclusive(in, st, primary, canary, weight)
		}
		return hold(PhaseProgressing, inferencev1alpha1.ReasonAnalysisInconclusive,
			"Analysis inconclusive ("+itoa(st.consecInconcl)+"/"+itoa(in.Plan.InconclusiveLimit)+
				"); most often too little traffic to judge", in.Plan.Interval)

	case inferencev1alpha1.VerdictFail:
		if st.failed >= in.Plan.FailureThreshold {
			return rollback(in, st, inferencev1alpha1.ReasonCanaryRolledBack,
				"Rolling back: "+itoa(st.failed)+" failed checks reached the threshold of "+
					itoa(in.Plan.FailureThreshold))
		}
		return hold(PhaseProgressing, inferencev1alpha1.ReasonCanaryChecksFailed,
			"Check failed ("+itoa(st.failed)+"/"+itoa(in.Plan.FailureThreshold)+
				"); holding at the current weight", in.Plan.Interval)

	default: // Pass
		return advance(in, st)
	}
}

// advance moves to the next rung, or promotes if the ladder is finished.
func advance(in Input, st State) Output {
	last := int32(len(in.Plan.Weights)) - 1

	if st.Step >= last {
		if in.Plan.RequireApproval {
			st.Phase = PhasePaused
			weight := currentWeight(in.Plan, st)
			primary, canary := splitFor(in, weight)
			return Output{
				State:          st,
				Action:         ActionPause,
				DesiredWeight:  weight,
				Primary:        primary,
				Canary:         canary,
				RealizedWeight: RealizedWeight(primary, canary),
				Reason:         inferencev1alpha1.ReasonAwaitingApproval,
				Message: "Every check passed at the final weight. Set " + annoPromote +
					"=\"true\" to promote, or " + annoAbort + "=\"true\" to roll back.",
			}
		}

		st.Phase = PhasePromoting
		return Output{
			State:          st,
			Action:         ActionPromote,
			DesiredWeight:  100,
			Primary:        in.TotalReplicas,
			RealizedWeight: 100,
			Reason:         inferencev1alpha1.ReasonCanaryPromoted,
			Message:        "Every check passed through the full weight ladder; promoting " + st.Revision,
		}
	}

	st.Step++
	st.Phase = PhaseProgressing
	weight := currentWeight(in.Plan, st)
	primary, canary := splitFor(in, weight)

	return Output{
		State:          st,
		Action:         ActionAdvance,
		DesiredWeight:  weight,
		Primary:        primary,
		Canary:         canary,
		RealizedWeight: RealizedWeight(primary, canary),
		RequeueAfter:   in.Plan.Interval,
		Reason:         inferencev1alpha1.ReasonCanaryChecksPassed,
		Message:        "Checks passed; advancing to " + itoa(weight) + "% traffic",
	}
}

// onInconclusive applies the configured policy once the limit is reached.
func onInconclusive(in Input, st State, primary, canary, weight int32) Output {
	switch in.Plan.OnInconclusive {
	case inferencev1alpha1.InconclusiveRollback:
		return rollback(in, st, inferencev1alpha1.ReasonAnalysisInconclusive,
			"Rolling back: analysis was inconclusive for "+itoa(st.consecInconcl)+" consecutive rounds")

	case inferencev1alpha1.InconclusivePromote:
		st.Phase = PhasePromoting
		return Output{
			State:          st,
			Action:         ActionPromote,
			DesiredWeight:  100,
			Primary:        in.TotalReplicas,
			RealizedWeight: 100,
			Reason:         inferencev1alpha1.ReasonCanaryPromoted,
			Message: "Promoting despite inconclusive analysis, as onInconclusive: Promote requests. " +
				"No metric evidence supports this promotion.",
		}

	default: // Wait
		// Hold indefinitely. "We cannot tell" is neither evidence of health nor
		// evidence of harm; freezing the blast radius and leaving the decision
		// to a human is what should happen when the automation has run out of
		// information.
		st.Phase = PhaseProgressing
		return Output{
			State:          st,
			Action:         ActionWait,
			DesiredWeight:  weight,
			Primary:        primary,
			Canary:         canary,
			RealizedWeight: RealizedWeight(primary, canary),
			RequeueAfter:   in.Plan.Interval,
			Reason:         inferencev1alpha1.ReasonAnalysisInconclusive,
			Message: "Held at " + itoa(weight) + "% after " + itoa(st.consecInconcl) +
				" inconclusive rounds. Most often there is too little traffic to judge; " +
				"send load, raise the analysis window, or lower minRequestRate.",
		}
	}
}

// rollback tears the canary down and restores the stable revision.
func rollback(in Input, st State, reason, message string) Output {
	st.Phase = PhaseRollingBack
	// Remember what was rejected, so the next pass does not start the identical
	// canary over again.
	st.FailedRevision = st.Revision
	return Output{
		State:          st,
		Action:         ActionRollback,
		DesiredWeight:  0,
		Primary:        in.TotalReplicas,
		Canary:         0,
		RealizedWeight: 0,
		Reason:         reason,
		Message:        message,
	}
}

// splitFor distributes the total replica count for a weight, honouring an
// explicit canary size.
func splitFor(in Input, weight int32) (primary, canary int32) {
	return CanaryReplicas(in.TotalReplicas, weight, in.Plan.CanaryReplicaOverride)
}

// weightAt returns the ladder's weight at an index, clamped into range.
func weightAt(p Plan, step int32) int32 {
	if len(p.Weights) == 0 {
		return 0
	}
	step = max(step, 0)
	if int(step) >= len(p.Weights) {
		step = int32(len(p.Weights)) - 1
	}
	return p.Weights[step]
}

// currentWeight returns the weight for the state's current step.
func currentWeight(p Plan, st State) int32 { return weightAt(p, st.Step) }

// nextAnalysisIn returns how long until the next round is due.
func nextAnalysisIn(in Input, st State) time.Duration {
	if st.LastAnalysis.IsZero() {
		return in.Plan.Interval
	}
	remaining := st.LastAnalysis.Add(in.Plan.Interval).Sub(in.Now)
	if remaining <= 0 {
		// Due already. A zero RequeueAfter means "no timer", which would leave
		// the rollout waiting for an unrelated event, so return the smallest
		// meaningful delay instead.
		return time.Second
	}
	return remaining
}

// isActive reports whether a phase describes a canary in flight.
func isActive(p Phase) bool {
	switch p {
	case PhaseWaiting, PhaseProgressing, PhasePaused:
		return true
	default:
		return false
	}
}

// itoa formats a small non-negative int32 for a message.
func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
