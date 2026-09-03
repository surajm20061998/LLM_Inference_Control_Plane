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
	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Round is the aggregate outcome of one analysis pass over every metric.
type Round struct {
	// Verdict is the round's single outcome, collapsed from the per-metric
	// checks by Aggregate.
	Verdict inferencev1alpha1.Verdict

	// Checks are the individual results, preserved for status so a human can
	// see WHICH metric decided the round.
	Checks []inferencev1alpha1.MetricCheck
}

// Aggregate collapses per-metric verdicts into one round verdict.
//
// # The precedence order is the whole safety argument
//
//	Error > Fail > Inconclusive > Pass
//
// Read it as "the most alarming thing that happened decides", with one crucial
// exception baked into the ordering: ERROR OUTRANKS FAIL.
//
// That ordering is counter-intuitive at first — a failure sounds worse than an
// error — and it is the single most important decision in this file. Suppose
// three metrics are checked and Prometheus is half-broken: one query times out,
// another returns a partial result that happens to look bad. If Fail outranked
// Error, that round counts toward the rollback budget and, repeated, tears down
// a canary because the monitoring stack is sick. With Error outranking Fail,
// the round counts toward the ERROR budget instead, which resets on the first
// healthy scrape and never rolls anything back.
//
// The cost of this ordering is a real failure being masked while Prometheus is
// also unhealthy. That is the right trade: an unnoticed regression is
// recoverable by the next round, whereas a spurious rollback destroys the
// deployment the operator was in the middle of and erases the evidence.
//
// An empty check list is Error, not Pass. "No metrics were evaluated" must
// never be a promotion.
func Aggregate(checks []inferencev1alpha1.MetricCheck) Round {
	if len(checks) == 0 {
		return Round{
			Verdict: inferencev1alpha1.VerdictError,
			Checks:  nil,
		}
	}

	worst := inferencev1alpha1.VerdictPass
	for _, c := range checks {
		if rank(c.Verdict) > rank(worst) {
			worst = c.Verdict
		}
	}

	return Round{Verdict: worst, Checks: checks}
}

// rank orders verdicts by how strongly they should dominate a round.
func rank(v inferencev1alpha1.Verdict) int {
	switch v {
	case inferencev1alpha1.VerdictPass:
		return 0
	case inferencev1alpha1.VerdictInconclusive:
		return 1
	case inferencev1alpha1.VerdictFail:
		return 2
	case inferencev1alpha1.VerdictError:
		return 3
	default:
		// An unrecognised verdict is treated as the most alarming thing that
		// could have happened. A new enum value reaching this code means the
		// API and the controller are out of step, and guessing "probably fine"
		// about a value nobody has taught it to read is how a rollout gate
		// becomes decorative.
		return 4
	}
}

// counters tracks the three disjoint budgets a canary spends.
//
// Disjoint is the operative word. A single "strikes" counter is how homegrown
// canary controllers roll back for the wrong reason: it cannot distinguish
// "the release is bad" from "Prometheus is down" from "nobody is sending
// traffic", and those three call for a rollback, a retry and a human
// respectively.
type counters struct {
	failed        int32
	consecErr     int32
	consecInconcl int32
}

// apply folds one round's verdict into the counters and returns the result.
//
// The asymmetry in what a Pass resets is deliberate and follows Flagger:
//
//   - A Pass RESETS the error and inconclusive counters. Both describe the
//     measurement apparatus rather than the release, and a successful
//     measurement is direct evidence the apparatus recovered.
//
//   - A Pass does NOT reset the failure counter. A canary that fails, passes,
//     fails, passes, fails is not healthy — it is intermittently broken, and
//     for a latency SLI that is the more common shape of a real regression than
//     a clean step change. Resetting here would make such a canary immortal: it
//     would climb the ladder forever, never accumulating enough consecutive
//     failures to be stopped.
func (c counters) apply(v inferencev1alpha1.Verdict) counters {
	switch v {
	case inferencev1alpha1.VerdictPass:
		c.consecErr = 0
		c.consecInconcl = 0
	case inferencev1alpha1.VerdictFail:
		c.failed++
		c.consecErr = 0
		c.consecInconcl = 0
	case inferencev1alpha1.VerdictError:
		c.consecErr++
	case inferencev1alpha1.VerdictInconclusive:
		c.consecInconcl++
		// Errors are reset: the provider answered, it just answered something
		// unusable. That is a traffic problem, not a monitoring problem, and
		// conflating them would let a quiet service exhaust the error budget
		// and abort for a reason that has nothing to do with either.
		c.consecErr = 0
	}
	return c
}

// Accessors for the embedded counters.
//
// The counters themselves are unexported so that only apply can change them:
// the reset rules — a Pass clears errors and inconclusives but never failures —
// are the whole safety argument, and a caller able to assign the fields
// directly could bypass them without the compiler noticing.

// FailedChecks returns how many rounds have failed.
func (s State) FailedChecks() int32 { return s.failed }

// ConsecutiveErrors returns the current run of provider failures.
func (s State) ConsecutiveErrors() int32 { return s.consecErr }

// ConsecutiveInconclusive returns the current run of unusable rounds.
func (s State) ConsecutiveInconclusive() int32 { return s.consecInconcl }

// SetCounters restores the budgets from persisted status.
//
// It exists for exactly one caller: the controller rebuilding State from
// status.canary after a restart. Without it a leader-election handover would
// silently reset the failure budget, and a canary that had already failed twice
// would get three more chances every time the operator was rescheduled.
func (s *State) SetCounters(failed, consecutiveErrors, consecutiveInconclusive int32) {
	s.failed = max(failed, 0)
	s.consecErr = max(consecutiveErrors, 0)
	s.consecInconcl = max(consecutiveInconclusive, 0)
}
