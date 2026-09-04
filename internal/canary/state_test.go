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

package canary

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Fixed instants. Wall-clock time never enters this package, so an entire
// multi-step rollout replays in microseconds and never flakes on a busy runner.
var (
	t0 = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
)

const (
	revStable = "stable01"
	revTarget = "target01"
	revOther  = "other001"
)

// testPlan is the plan every case starts from: a three-rung ladder, a 30s
// interval and a 60s warm-up — the values the feasibility spike measured.
func testPlan(mutate ...func(*Plan)) Plan {
	p := Plan{
		Weights:               []int32{20, 40, 50},
		Interval:              30 * time.Second,
		InitialDelay:          60 * time.Second,
		FailureThreshold:      3,
		ConsecutiveErrorLimit: 5,
		InconclusiveLimit:     5,
		OnInconclusive:        inferencev1alpha1.InconclusiveWait,
	}
	for _, m := range mutate {
		m(&p)
	}
	return p
}

// round builds an analysis outcome with a single check of the given verdict.
func round(v inferencev1alpha1.Verdict) *Round {
	return &Round{
		Verdict: v,
		Checks:  []inferencev1alpha1.MetricCheck{{Name: "ttft", Verdict: v}},
	}
}

// warmState is a canary that has been running long enough to be analysed.
func warmState(step int32, mutate ...func(*State)) State {
	st := State{
		Phase:          PhaseProgressing,
		Revision:       revTarget,
		StableRevision: revStable,
		Step:           step,
		StartedAt:      t0,
		AvailableSince: t0,
	}
	for _, m := range mutate {
		m(&st)
	}
	return st
}

// analysed returns an input positioned just after the warm-up, carrying a
// verdict.
func analysed(plan Plan, st State, verdict inferencev1alpha1.Verdict, mutate ...func(*Input)) Input {
	in := Input{
		Now:             t0.Add(plan.InitialDelay + time.Second),
		Plan:            plan,
		State:           st,
		TargetRevision:  revTarget,
		StableRevision:  revStable,
		TotalReplicas:   10,
		CanaryAvailable: true,
		Round:           round(verdict),
	}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

func TestNextTable(t *testing.T) {
	t.Parallel()

	plan := testPlan()

	cases := []struct {
		name string
		in   Input

		wantAction  Action
		wantPhase   Phase
		wantStep    int32
		wantWeight  int32
		wantReason  string
		wantFailed  int32
		wantRequeue time.Duration
	}{
		// ---- Starting ---------------------------------------------------
		{
			name: "no change means no canary",
			in: Input{
				Now: t0, Plan: plan,
				TargetRevision: revStable, StableRevision: revStable, TotalReplicas: 4,
			},
			wantAction: ActionNone, wantPhase: PhaseIdle,
			wantReason: inferencev1alpha1.ReasonCanaryNotRunning,
		},
		{
			name: "a new revision starts a canary at the first rung",
			in: Input{
				Now: t0, Plan: plan,
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
			},
			wantAction: ActionStart, wantPhase: PhaseWaiting, wantWeight: 20,
			wantReason: inferencev1alpha1.ReasonCanaryProgressing, wantRequeue: plan.Interval,
		},
		{
			// A canary with nothing to fall back to is a deploy with extra
			// steps. The revision still rolls out; it is simply not gated.
			name: "no stable revision rolls out directly",
			in: Input{
				Now: t0, Plan: plan,
				TargetRevision: revTarget, StableRevision: "", TotalReplicas: 10,
			},
			wantAction: ActionNone, wantPhase: PhaseIdle,
			wantReason: inferencev1alpha1.ReasonNoLastGoodRevision,
		},
		{
			name: "one replica cannot be split, so no canary runs",
			in: Input{
				Now: t0, Plan: plan,
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 1,
			},
			wantAction: ActionNone, wantPhase: PhaseIdle,
			wantReason: inferencev1alpha1.ReasonInsufficientReplicas,
		},
		{
			name: "an empty ladder promotes immediately",
			in: Input{
				Now: t0, Plan: testPlan(func(p *Plan) { p.Weights = nil }),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
			},
			wantAction: ActionPromote, wantPhase: PhasePromoting, wantWeight: 100, wantRequeue: plan.Interval,
			wantReason: inferencev1alpha1.ReasonCanaryPromoted,
		},
		{
			// A rollback must STICK. Without this the machine restarts an
			// identical canary immediately and loops forever.
			name: "a rolled-back revision is not retried",
			in: Input{
				Now: t0, Plan: plan,
				State:          State{Phase: PhaseIdle, FailedRevision: revTarget},
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
			},
			wantAction: ActionNone, wantPhase: PhaseIdle,
			wantReason: inferencev1alpha1.ReasonCanaryRolledBack,
		},

		// ---- Waiting and warming ----------------------------------------
		{
			name: "unavailable canary holds without analysing",
			in: Input{
				Now: t0.Add(time.Minute), Plan: plan,
				State:          warmState(0),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: false,
			},
			wantAction: ActionWait, wantPhase: PhaseWaiting, wantWeight: 20,
			wantReason: inferencev1alpha1.ReasonCanaryProgressing, wantRequeue: plan.Interval,
		},
		{
			// ...but not forever. A canary whose pods never start produces no
			// traffic, so no analysis round is ever due, so no budget is spent
			// and no verdict is ever reached. Without a deadline the rollout
			// simply hangs in Canarying — and the controller's stall guard
			// cannot rescue it, because that guard reads the PRIMARY Deployment
			// and is skipped outright while a canary is active.
			name: "a canary that never becomes available is rolled back at the deadline",
			in: Input{
				Now: t0.Add(11 * time.Minute),
				Plan: testPlan(func(p *Plan) {
					p.ProgressDeadline = 10 * time.Minute
				}),
				State:          warmState(0),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: false,
			},
			wantAction: ActionRollback, wantPhase: PhaseRollingBack,
			wantReason: inferencev1alpha1.ReasonCanaryProgressDeadlineExceeded,
		},
		{
			name: "an unavailable canary still inside the deadline keeps waiting",
			in: Input{
				Now: t0.Add(9 * time.Minute),
				Plan: testPlan(func(p *Plan) {
					p.ProgressDeadline = 10 * time.Minute
				}),
				State:          warmState(0),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: false,
			},
			wantAction: ActionWait, wantPhase: PhaseWaiting, wantWeight: 20,
			wantReason: inferencev1alpha1.ReasonCanaryProgressing, wantRequeue: plan.Interval,
		},
		{
			// A cold inference pod has a terrible, unrepresentative TTFT.
			// Measuring it would fail every canary that was going to be fine.
			name: "warm-up delay suppresses analysis",
			in: Input{
				Now: t0.Add(10 * time.Second), Plan: plan,
				State:          warmState(0),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: true, Round: round(inferencev1alpha1.VerdictFail),
			},
			wantAction: ActionWait, wantPhase: PhaseProgressing, wantWeight: 20,
			wantReason:  inferencev1alpha1.ReasonCanaryProgressing,
			wantRequeue: 50 * time.Second,
		},

		// ---- Passing and advancing --------------------------------------
		{
			name:       "a pass advances one rung",
			in:         analysed(plan, warmState(0), inferencev1alpha1.VerdictPass),
			wantAction: ActionAdvance, wantPhase: PhaseProgressing, wantStep: 1, wantWeight: 40,
			wantReason: inferencev1alpha1.ReasonCanaryChecksPassed, wantRequeue: plan.Interval,
		},
		{
			name:       "a pass at the last rung promotes",
			in:         analysed(plan, warmState(2), inferencev1alpha1.VerdictPass),
			wantAction: ActionPromote, wantPhase: PhasePromoting, wantStep: 2, wantWeight: 100, wantRequeue: plan.Interval,
			wantReason: inferencev1alpha1.ReasonCanaryPromoted,
		},
		{
			name: "a pass at the last rung pauses when approval is required",
			in: analysed(testPlan(func(p *Plan) { p.RequireApproval = true }),
				warmState(2), inferencev1alpha1.VerdictPass),
			wantAction: ActionPause, wantPhase: PhasePaused, wantStep: 2, wantWeight: 50,
			wantReason: inferencev1alpha1.ReasonAwaitingApproval,
		},

		// ---- Failing ------------------------------------------------------
		{
			name:       "the first failure holds rather than rolling back",
			in:         analysed(plan, warmState(1), inferencev1alpha1.VerdictFail),
			wantAction: ActionWait, wantPhase: PhaseProgressing, wantStep: 1, wantWeight: 40,
			wantReason: inferencev1alpha1.ReasonCanaryChecksFailed, wantFailed: 1,
			wantRequeue: plan.Interval,
		},
		{
			name: "reaching the failure threshold rolls back",
			in: analysed(plan, warmState(1, func(s *State) { s.SetCounters(2, 0, 0) }),
				inferencev1alpha1.VerdictFail),
			wantAction: ActionRollback, wantPhase: PhaseRollingBack, wantStep: 1, wantWeight: 0,
			wantReason: inferencev1alpha1.ReasonCanaryRolledBack, wantFailed: 3,
		},

		// ---- Provider errors ----------------------------------------------
		{
			// THE safety property: a monitoring outage must never roll back a
			// production deployment.
			name:       "a provider error holds and never rolls back",
			in:         analysed(plan, warmState(1), inferencev1alpha1.VerdictError),
			wantAction: ActionWait, wantPhase: PhaseProgressing, wantStep: 1, wantWeight: 40,
			wantReason: inferencev1alpha1.ReasonAnalysisError, wantRequeue: plan.Interval,
		},
		{
			// It does eventually abort, but the reason says AnalysisError, not
			// ChecksFailed: the canary was never judged bad.
			name: "the consecutive-error limit aborts, blaming the provider",
			in: analysed(plan, warmState(1, func(s *State) { s.SetCounters(0, 4, 0) }),
				inferencev1alpha1.VerdictError),
			wantAction: ActionRollback, wantPhase: PhaseRollingBack, wantStep: 1,
			wantReason: inferencev1alpha1.ReasonAnalysisError,
		},

		// ---- Inconclusive ---------------------------------------------------
		{
			name:       "an inconclusive round holds",
			in:         analysed(plan, warmState(0), inferencev1alpha1.VerdictInconclusive),
			wantAction: ActionWait, wantPhase: PhaseProgressing, wantWeight: 20,
			wantReason: inferencev1alpha1.ReasonAnalysisInconclusive, wantRequeue: plan.Interval,
		},
		{
			name: "onInconclusive Wait holds indefinitely at the limit",
			in: analysed(plan, warmState(0, func(s *State) { s.SetCounters(0, 0, 4) }),
				inferencev1alpha1.VerdictInconclusive),
			wantAction: ActionWait, wantPhase: PhaseProgressing, wantWeight: 20,
			wantReason: inferencev1alpha1.ReasonAnalysisInconclusive, wantRequeue: plan.Interval,
		},
		{
			name: "onInconclusive Rollback reverts at the limit",
			in: analysed(testPlan(func(p *Plan) { p.OnInconclusive = inferencev1alpha1.InconclusiveRollback }),
				warmState(0, func(s *State) { s.SetCounters(0, 0, 4) }),
				inferencev1alpha1.VerdictInconclusive),
			wantAction: ActionRollback, wantPhase: PhaseRollingBack,
			wantReason: inferencev1alpha1.ReasonAnalysisInconclusive,
		},
		{
			name: "onInconclusive Promote continues at the limit",
			in: analysed(testPlan(func(p *Plan) { p.OnInconclusive = inferencev1alpha1.InconclusivePromote }),
				warmState(0, func(s *State) { s.SetCounters(0, 0, 4) }),
				inferencev1alpha1.VerdictInconclusive),
			wantAction: ActionPromote, wantPhase: PhasePromoting, wantWeight: 100, wantRequeue: plan.Interval,
			wantReason: inferencev1alpha1.ReasonCanaryPromoted,
		},

		// ---- External signals ------------------------------------------------
		{
			name: "abort rolls back immediately",
			in: analysed(plan, warmState(1), inferencev1alpha1.VerdictPass,
				func(i *Input) { i.Aborted = true }),
			wantAction: ActionRollback, wantPhase: PhaseRollingBack, wantStep: 1,
			wantReason: inferencev1alpha1.ReasonCanaryAborted,
		},
		{
			// An operator hitting the brakes must not race with the machine's
			// own optimism.
			name: "abort outranks approval in the same pass",
			in: Input{
				Now: t0, Plan: plan,
				State:          warmState(2, func(s *State) { s.Phase = PhasePaused }),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: true, Approved: true, Aborted: true,
			},
			wantAction: ActionRollback, wantPhase: PhaseRollingBack, wantStep: 2,
			wantReason: inferencev1alpha1.ReasonCanaryAborted,
		},
		{
			name: "approval promotes from paused",
			in: Input{
				Now: t0, Plan: plan,
				State:          warmState(2, func(s *State) { s.Phase = PhasePaused }),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: true, Approved: true,
			},
			wantAction: ActionPromote, wantPhase: PhasePromoting, wantStep: 2, wantWeight: 100, wantRequeue: plan.Interval,
			wantReason: inferencev1alpha1.ReasonCanaryPromoted,
		},
		{
			// Nothing but external input leaves a pause. A timeout here would
			// make the gate advisory.
			name: "a pause with no signal stays paused and sets no timer",
			in: Input{
				Now: t0.Add(time.Hour), Plan: plan,
				State:          warmState(2, func(s *State) { s.Phase = PhasePaused }),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: true,
			},
			wantAction: ActionPause, wantPhase: PhasePaused, wantStep: 2, wantWeight: 50,
			wantReason: inferencev1alpha1.ReasonAwaitingApproval, wantRequeue: 0,
		},

		// ---- Mid-flight spec change -------------------------------------------
		{
			// Carrying the failure count and the current weight across to a
			// revision that has never been measured would promote a new build
			// on the strength of checks run against a different one.
			name: "a spec change restarts at step zero",
			in: Input{
				Now: t0, Plan: plan,
				State:          warmState(2, func(s *State) { s.SetCounters(2, 0, 0) }),
				TargetRevision: revOther, StableRevision: revStable, TotalReplicas: 10,
				CanaryAvailable: true,
			},
			wantAction: ActionStart, wantPhase: PhaseWaiting, wantStep: 0, wantWeight: 20,
			wantReason: inferencev1alpha1.ReasonCanaryProgressing, wantRequeue: plan.Interval,
		},

		// ---- Promotion convergence ----------------------------------------------
		{
			// Promotion is not atomic: the primary still has to roll out. Until
			// it does, re-asserting the same desired state is the only safe
			// answer — anything else starts a canary for the revision just
			// promoted.
			name: "promotion holds until the primary converges",
			in: Input{
				Now: t0, Plan: plan,
				State:          warmState(2, func(s *State) { s.Phase = PhasePromoting }),
				TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
			},
			wantAction: ActionPromote, wantPhase: PhasePromoting, wantStep: 2, wantWeight: 100, wantRequeue: plan.Interval,
			wantReason: inferencev1alpha1.ReasonCanaryPromoted,
		},
		{
			name: "promotion completes once the stable revision catches up",
			in: Input{
				Now: t0, Plan: plan,
				State:          warmState(2, func(s *State) { s.Phase = PhasePromoting }),
				TargetRevision: revTarget, StableRevision: revTarget, TotalReplicas: 10,
			},
			wantAction: ActionNone, wantPhase: PhaseIdle,
			wantReason: inferencev1alpha1.ReasonCanaryPromoted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Next(tc.in)

			if got.Action != tc.wantAction {
				t.Errorf("action = %q, want %q (%s)", got.Action, tc.wantAction, got.Message)
			}
			if got.State.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", got.State.Phase, tc.wantPhase)
			}
			if got.State.Step != tc.wantStep {
				t.Errorf("step = %d, want %d", got.State.Step, tc.wantStep)
			}
			if got.DesiredWeight != tc.wantWeight {
				t.Errorf("desiredWeight = %d, want %d", got.DesiredWeight, tc.wantWeight)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.State.FailedChecks() != tc.wantFailed {
				t.Errorf("failedChecks = %d, want %d", got.State.FailedChecks(), tc.wantFailed)
			}
			if got.RequeueAfter != tc.wantRequeue {
				t.Errorf("requeueAfter = %v, want %v", got.RequeueAfter, tc.wantRequeue)
			}
		})
	}
}

func TestPassDoesNotResetTheFailureCounter(t *testing.T) {
	t.Parallel()

	// Flagger semantics, and the reason matters: a canary that fails, passes,
	// fails, passes, fails is not healthy, it is intermittently broken — which
	// for a latency SLI is the more common shape of a real regression than a
	// clean step change. Resetting here would make such a canary immortal.
	plan := testPlan()
	st := warmState(0, func(s *State) { s.SetCounters(2, 0, 0) })

	out := Next(analysed(plan, st, inferencev1alpha1.VerdictPass))
	if got := out.State.FailedChecks(); got != 2 {
		t.Fatalf("failedChecks = %d after a pass, want 2 — a pass must not forgive earlier failures", got)
	}
	if out.Action != ActionAdvance {
		t.Fatalf("action = %q, want Advance", out.Action)
	}
}

func TestPassResetsTheErrorAndInconclusiveCounters(t *testing.T) {
	t.Parallel()

	// These two describe the measurement apparatus rather than the release, and
	// a successful measurement is direct evidence the apparatus recovered.
	plan := testPlan()
	st := warmState(0, func(s *State) { s.SetCounters(0, 3, 3) })

	out := Next(analysed(plan, st, inferencev1alpha1.VerdictPass))
	if out.State.ConsecutiveErrors() != 0 {
		t.Errorf("consecutiveErrors = %d after a pass, want 0", out.State.ConsecutiveErrors())
	}
	if out.State.ConsecutiveInconclusive() != 0 {
		t.Errorf("consecutiveInconclusive = %d after a pass, want 0", out.State.ConsecutiveInconclusive())
	}
}

func TestRollbackRecordsTheFailedRevision(t *testing.T) {
	t.Parallel()

	plan := testPlan()
	st := warmState(1, func(s *State) { s.SetCounters(2, 0, 0) })

	out := Next(analysed(plan, st, inferencev1alpha1.VerdictFail))
	if out.State.FailedRevision != revTarget {
		t.Fatalf("failedRevision = %q, want %q; without it the next pass starts an identical canary",
			out.State.FailedRevision, revTarget)
	}
}

// TestFullLadderPromotes replays an entire successful rollout, driving the
// clock forward exactly as the returned RequeueAfter asks.
//
// This is the payoff of a pure state machine: a five-round canary with a 60s
// warm-up and 30s intervals — three and a half minutes of wall time — runs here
// in microseconds and is byte-for-byte deterministic.
func TestFullLadderPromotes(t *testing.T) {
	t.Parallel()

	plan := testPlan()
	now := t0

	in := Input{
		Now: now, Plan: plan,
		TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
	}

	out := Next(in)
	if out.Action != ActionStart {
		t.Fatalf("round 0: action = %q, want Start", out.Action)
	}

	wantWeights := []int32{20, 40, 50}
	state := out.State

	for i, want := range wantWeights {
		// The canary becomes available, then the warm-up elapses.
		now = now.Add(plan.InitialDelay + time.Second)

		in = Input{
			Now: now, Plan: plan, State: state,
			TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
			CanaryAvailable: true,
		}
		if state.AvailableSince.IsZero() {
			// The first pass records availability; supply it as the controller
			// would from the canary Deployment's status.
			in.State.AvailableSince = t0
		}

		if !DueForAnalysis(in.Now, plan, in.State) {
			t.Fatalf("round %d: analysis should be due at %v", i, in.Now)
		}
		in.Round = round(inferencev1alpha1.VerdictPass)

		out = Next(in)
		state = out.State

		if i < len(wantWeights)-1 {
			if out.Action != ActionAdvance {
				t.Fatalf("round %d: action = %q, want Advance (%s)", i, out.Action, out.Message)
			}
			if out.DesiredWeight != wantWeights[i+1] {
				t.Fatalf("round %d: advanced to %d%%, want %d%%", i, out.DesiredWeight, wantWeights[i+1])
			}
			continue
		}

		if out.Action != ActionPromote {
			t.Fatalf("final round: action = %q, want Promote (%s)", out.Action, out.Message)
		}
		_ = want
	}

	// Convergence: the primary catches up and the canary settles.
	out = Next(Input{
		Now: now, Plan: plan, State: state,
		TargetRevision: revTarget, StableRevision: revTarget, TotalReplicas: 10,
	})
	if out.Action != ActionNone || out.State.Phase != PhaseIdle {
		t.Fatalf("after convergence: action = %q phase = %q, want None/Idle", out.Action, out.State.Phase)
	}
}

// TestRollbackOnExactlySecondFailure is the assertion the scripted-fake design
// exists for.
//
// With a failureThreshold of 2, the rollback must fire on the second failure —
// not the first, and not the third. Against a live Prometheus this cannot be
// asserted at all: you cannot make a real metric fail precisely twice.
func TestRollbackOnExactlySecondFailure(t *testing.T) {
	t.Parallel()

	plan := testPlan(func(p *Plan) { p.FailureThreshold = 2 })
	state := warmState(0)
	now := t0.Add(plan.InitialDelay + time.Second)

	step := func(v inferencev1alpha1.Verdict) Output {
		out := Next(Input{
			Now: now, Plan: plan, State: state,
			TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
			CanaryAvailable: true, Round: round(v),
		})
		state = out.State
		now = now.Add(plan.Interval)
		return out
	}

	if out := step(inferencev1alpha1.VerdictPass); out.Action != ActionAdvance {
		t.Fatalf("pass 1: action = %q, want Advance", out.Action)
	}
	if out := step(inferencev1alpha1.VerdictPass); out.Action != ActionAdvance {
		t.Fatalf("pass 2: action = %q, want Advance", out.Action)
	}
	if out := step(inferencev1alpha1.VerdictFail); out.Action != ActionWait {
		t.Fatalf("fail 1: action = %q, want Wait — one failure is below the threshold", out.Action)
	}
	out := step(inferencev1alpha1.VerdictFail)
	if out.Action != ActionRollback {
		t.Fatalf("fail 2: action = %q, want Rollback at exactly the threshold", out.Action)
	}
	if out.State.FailedChecks() != 2 {
		t.Fatalf("failedChecks = %d, want exactly 2", out.State.FailedChecks())
	}
}

// TestNextIsIdempotent checks the property the reconcile loop depends on: the
// controller may call Next any number of times between two real changes.
func TestNextIsIdempotent(t *testing.T) {
	t.Parallel()

	plan := testPlan()
	inputs := []Input{
		{Now: t0, Plan: plan, TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10},
		{Now: t0, Plan: plan, State: warmState(1), TargetRevision: revTarget,
			StableRevision: revStable, TotalReplicas: 10, CanaryAvailable: true},
		{Now: t0, Plan: plan, State: warmState(2, func(s *State) { s.Phase = PhasePaused }),
			TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10, CanaryAvailable: true},
	}

	for i, in := range inputs {
		first := Next(in)
		for range 50 {
			if got := Next(in); got != first {
				t.Fatalf("input %d: Next is not deterministic\nfirst: %+v\ngot:   %+v", i, first, got)
			}
		}

		// Feeding the output's state straight back must not move anything,
		// provided no other input changed.
		in.State = first.State
		second := Next(in)
		if second.Action == ActionAdvance || second.Action == ActionStart {
			// Re-supplying the same Round would double-count it; the controller
			// never does, so neither does this check.
			if in.Round != nil {
				continue
			}
			t.Fatalf("input %d: re-running on the produced state moved to %q", i, second.Action)
		}
	}
}

// TestWeightsAreMonotonic is the invariant a rollout's blast radius depends on:
// exposure never decreases while a canary is progressing.
func TestWeightsAreMonotonic(t *testing.T) {
	t.Parallel()

	plan := testPlan(func(p *Plan) { p.Weights = []int32{5, 10, 25, 50, 75} })
	state := warmState(0)
	now := t0.Add(plan.InitialDelay + time.Second)

	last := int32(0)
	for range len(plan.Weights) {
		out := Next(Input{
			Now: now, Plan: plan, State: state,
			TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 20,
			CanaryAvailable: true, Round: round(inferencev1alpha1.VerdictPass),
		})
		if out.Action == ActionPromote {
			break
		}
		if out.DesiredWeight < last {
			t.Fatalf("weight went backwards: %d after %d", out.DesiredWeight, last)
		}
		last = out.DesiredWeight
		state = out.State
		now = now.Add(plan.Interval)
	}
}

func TestDueForAnalysis(t *testing.T) {
	t.Parallel()

	plan := testPlan()

	cases := []struct {
		name  string
		now   time.Time
		state State
		want  bool
	}{
		{
			name: "idle is never analysed",
			now:  t0.Add(time.Hour), state: State{Phase: PhaseIdle}, want: false,
		},
		{
			name:  "paused is never analysed",
			now:   t0.Add(time.Hour),
			state: State{Phase: PhasePaused, AvailableSince: t0},
			want:  false,
		},
		{
			name: "not yet available is never analysed",
			now:  t0.Add(time.Hour), state: State{Phase: PhaseProgressing}, want: false,
		},
		{
			name:  "inside the warm-up window is not analysed",
			now:   t0.Add(30 * time.Second),
			state: State{Phase: PhaseProgressing, AvailableSince: t0},
			want:  false,
		},
		{
			name:  "the first round fires once the warm-up elapses",
			now:   t0.Add(plan.InitialDelay),
			state: State{Phase: PhaseProgressing, AvailableSince: t0},
			want:  true,
		},
		{
			name: "a round inside the interval does not fire",
			now:  t0.Add(plan.InitialDelay + 10*time.Second),
			state: State{Phase: PhaseProgressing, AvailableSince: t0,
				LastAnalysis: t0.Add(plan.InitialDelay)},
			want: false,
		},
		{
			name: "a round fires once the interval elapses",
			now:  t0.Add(plan.InitialDelay + plan.Interval),
			state: State{Phase: PhaseProgressing, AvailableSince: t0,
				LastAnalysis: t0.Add(plan.InitialDelay)},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DueForAnalysis(tc.now, plan, tc.state); got != tc.want {
				t.Errorf("DueForAnalysis = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanaryScaleOverrideIsHonoured(t *testing.T) {
	t.Parallel()

	// A fixed canary size means the pod is warm before it is measured, instead
	// of spending the first analysis window of every step loading a model.
	plan := testPlan(func(p *Plan) { p.CanaryReplicaOverride = ptr.To(int32(2)) })

	out := Next(Input{
		Now: t0, Plan: plan,
		TargetRevision: revTarget, StableRevision: revStable, TotalReplicas: 10,
	})
	if out.Canary != 2 || out.Primary != 8 {
		t.Fatalf("split = (%d, %d), want (8, 2)", out.Primary, out.Canary)
	}

	// And it does not change as the weight climbs.
	state := warmState(0)
	out = Next(analysed(plan, state, inferencev1alpha1.VerdictPass))
	if out.Canary != 2 {
		t.Fatalf("canary replicas = %d after advancing, want a constant 2", out.Canary)
	}
}

func TestAggregatePrecedence(t *testing.T) {
	t.Parallel()

	check := func(v inferencev1alpha1.Verdict) inferencev1alpha1.MetricCheck {
		return inferencev1alpha1.MetricCheck{Name: string(v), Verdict: v}
	}

	cases := []struct {
		name   string
		checks []inferencev1alpha1.MetricCheck
		want   inferencev1alpha1.Verdict
	}{
		{
			name: "no checks is an error, never a pass",
			want: inferencev1alpha1.VerdictError,
		},
		{
			name:   "all passing passes",
			checks: []inferencev1alpha1.MetricCheck{check(inferencev1alpha1.VerdictPass), check(inferencev1alpha1.VerdictPass)},
			want:   inferencev1alpha1.VerdictPass,
		},
		{
			name:   "inconclusive outranks pass",
			checks: []inferencev1alpha1.MetricCheck{check(inferencev1alpha1.VerdictPass), check(inferencev1alpha1.VerdictInconclusive)},
			want:   inferencev1alpha1.VerdictInconclusive,
		},
		{
			name:   "fail outranks inconclusive",
			checks: []inferencev1alpha1.MetricCheck{check(inferencev1alpha1.VerdictInconclusive), check(inferencev1alpha1.VerdictFail)},
			want:   inferencev1alpha1.VerdictFail,
		},
		{
			// The counter-intuitive one, and the most important. With Fail
			// outranking Error, a half-broken Prometheus would spend the
			// ROLLBACK budget; with Error winning it spends the error budget,
			// which resets on the first healthy scrape and never rolls back.
			name:   "error outranks fail",
			checks: []inferencev1alpha1.MetricCheck{check(inferencev1alpha1.VerdictFail), check(inferencev1alpha1.VerdictError)},
			want:   inferencev1alpha1.VerdictError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Aggregate(tc.checks).Verdict; got != tc.want {
				t.Errorf("Aggregate = %q, want %q", got, tc.want)
			}
		})
	}
}
