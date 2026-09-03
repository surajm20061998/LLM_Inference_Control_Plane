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

package autoscale

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// testConfig is the shape most cases start from: target 2 per pod, HPA's 0.1
// tolerance, instant scale-up and a five-minute scale-down window.
func testConfig(mutate ...func(*Config)) Config {
	c := Config{
		MinReplicas:            1,
		MaxReplicas:            10,
		Target:                 2,
		Tolerance:              0.1,
		ScaleUpStabilization:   0,
		ScaleDownStabilization: 5 * time.Minute,
	}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

// measured builds a valid observation.
func measured(v float64) Observation { return Observation{Value: v, Valid: true} }

func TestRecommendTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   Input

		wantReplicas  int32
		wantDirection Direction
		wantReason    string
		wantValid     bool
	}{
		{
			name: "at target, holds",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 4,
				Observation: measured(8), Config: testConfig(),
			},
			wantReplicas: 4, wantDirection: DirectionNone,
			wantReason: ReasonWithinTolerance, wantValid: true,
		},
		{
			// THE reason a tolerance band exists. 2.05 per pod against a target
			// of 2 is a ratio of 1.025; without the band it is a scale event,
			// and so is the next sample fifteen seconds later, forever.
			name: "just above target, inside the tolerance band, holds",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 4,
				Observation: measured(8.2), Config: testConfig(),
			},
			wantReplicas: 4, wantDirection: DirectionNone,
			wantReason: ReasonWithinTolerance, wantValid: true,
		},
		{
			name: "just below target, inside the tolerance band, holds",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 4,
				Observation: measured(7.4), Config: testConfig(),
			},
			wantReplicas: 4, wantDirection: DirectionNone,
			wantReason: ReasonWithinTolerance, wantValid: true,
		},
		{
			// 16/4 = 4 per pod, ratio 2.0, so 4 ready pods become 8.
			name: "double the target scales up proportionally",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 4,
				Observation: measured(16), Config: testConfig(),
			},
			wantReplicas: 8, wantDirection: DirectionUp,
			wantReason: ReasonScaleUp, wantValid: true,
		},
		{
			name: "the ceiling is respected",
			in: Input{
				Now: t0, Current: 8, ReadyReplicas: 8,
				Observation: measured(160), Config: testConfig(),
			},
			wantReplicas: 10, wantDirection: DirectionUp,
			wantReason: ReasonScaleUp, wantValid: true,
		},
		{
			// A fractional pod cannot serve, so the recommendation rounds up:
			// 4 ready x ratio 1.25 = 5.
			name: "a fractional recommendation rounds up",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 4,
				Observation: measured(10), Config: testConfig(),
			},
			wantReplicas: 5, wantDirection: DirectionUp,
			wantReason: ReasonScaleUp, wantValid: true,
		},
		{
			name: "an empty queue scales down to the floor",
			in: Input{
				Now: t0, Current: 6, ReadyReplicas: 6,
				Observation: measured(0), Config: testConfig(),
			},
			wantReplicas: 1, wantDirection: DirectionDown,
			wantReason: ReasonScaleDown, wantValid: true,
		},
		{
			name: "the floor is respected",
			in: Input{
				Now: t0, Current: 3, ReadyReplicas: 3,
				Observation: measured(0),
				Config:      testConfig(func(c *Config) { c.MinReplicas = 2 }),
			},
			wantReplicas: 2, wantDirection: DirectionDown,
			wantReason: ReasonScaleDown, wantValid: true,
		},
		{
			// An absent measurement is not a measurement of zero. Scaling on it
			// would size the fleet from a number nobody produced.
			name: "an invalid observation holds, it does not scale to the floor",
			in: Input{
				Now: t0, Current: 6, ReadyReplicas: 6,
				Observation: Observation{Valid: false, Reason: "provider unreachable"},
				Config:      testConfig(),
			},
			wantReplicas: 6, wantDirection: DirectionNone,
			wantReason: ReasonNoMetrics, wantValid: false,
		},
		{
			// The ratio's denominator would be zero, and a fleet with nothing
			// serving has no per-pod load to speak of.
			name: "no ready pods holds",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 0,
				Observation: measured(20), Config: testConfig(),
			},
			wantReplicas: 4, wantDirection: DirectionNone,
			wantReason: ReasonNoMetrics, wantValid: false,
		},
		{
			// Otherwise every ratio is infinite and the fleet goes straight to
			// maxReplicas.
			name: "a non-positive target holds",
			in: Input{
				Now: t0, Current: 4, ReadyReplicas: 4,
				Observation: measured(20),
				Config:      testConfig(func(c *Config) { c.Target = 0 }),
			},
			wantReplicas: 4, wantDirection: DirectionNone,
			wantReason: ReasonNoMetrics, wantValid: false,
		},
		{
			// Mid-scale-up: only 2 of 6 pods are ready. The recommendation is
			// derived from the queued demand, which is what it is regardless of
			// how many pods have finished loading — see the note on
			// Input.ReadyReplicas.
			name: "a partially-ready fleet is sized from demand, not from readiness",
			in: Input{
				Now: t0, Current: 6, ReadyReplicas: 2,
				Observation: measured(20), Config: testConfig(),
			},
			// 20 queued at a target of 2 per pod wants 10 pods.
			wantReplicas: 10, wantDirection: DirectionUp,
			wantReason: ReasonScaleUp, wantValid: true,
		},
		{
			// A spec whose bounds changed while the metric was unavailable is
			// still honoured; holding literally would leave the fleet outside
			// its own declared limits for the duration of the outage.
			name: "an invalid observation still clamps to the current bounds",
			in: Input{
				Now: t0, Current: 20, ReadyReplicas: 20,
				Observation: Observation{Valid: false},
				Config:      testConfig(),
			},
			wantReplicas: 10, wantDirection: DirectionDown,
			wantReason: ReasonNoMetrics, wantValid: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Recommend(tc.in)

			if got.Replicas != tc.wantReplicas {
				t.Errorf("replicas = %d, want %d (%s)", got.Replicas, tc.wantReplicas, got.Message)
			}
			if got.Direction != tc.wantDirection {
				t.Errorf("direction = %q, want %q", got.Direction, tc.wantDirection)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Valid != tc.wantValid {
				t.Errorf("valid = %v, want %v", got.Valid, tc.wantValid)
			}
			if got.Changed != (got.Replicas != tc.in.Current) {
				t.Errorf("changed = %v but replicas went %d -> %d",
					got.Changed, tc.in.Current, got.Replicas)
			}
		})
	}
}

func TestScaleDownStabilizationHoldsCapacity(t *testing.T) {
	t.Parallel()

	// A pod removed here has to load its weights again to come back, which for
	// a real model is minutes of unavailable capacity bought to save seconds of
	// idle capacity. The window has to actually hold.
	cfg := testConfig()

	// The fleet was at 6 under load a minute ago; demand has just dropped.
	history := []Recommendation{
		{Time: t0.Add(-4 * time.Minute), Replicas: 6},
		{Time: t0.Add(-3 * time.Minute), Replicas: 6},
		{Time: t0.Add(-1 * time.Minute), Replicas: 6},
	}

	out := Recommend(Input{
		Now: t0, Current: 6, ReadyReplicas: 6,
		Observation: measured(2), Config: cfg, History: history,
	})

	if out.Replicas != 6 {
		t.Fatalf("replicas = %d, want 6: the scale-down window has not cleared", out.Replicas)
	}
	if out.Raw != 1 {
		t.Errorf("raw = %d, want 1: the unstabilized recommendation must still be reported "+
			"so the difference between the metric and the damping is visible", out.Raw)
	}
	if out.Reason != ReasonStabilized {
		t.Errorf("reason = %q, want %q", out.Reason, ReasonStabilized)
	}

	// Once the window has passed with no high recommendations in it, the scale
	// down happens.
	later := t0.Add(6 * time.Minute)
	out = Recommend(Input{
		Now: later, Current: 6, ReadyReplicas: 6,
		Observation: measured(2), Config: cfg, History: history,
	})
	if out.Replicas != 1 {
		t.Fatalf("replicas = %d after the window elapsed, want 1", out.Replicas)
	}
}

func TestScaleUpIsNotDampedByDefault(t *testing.T) {
	t.Parallel()

	// The asymmetry is the point: adding capacity late costs latency the user
	// experiences, removing it early costs a cold start on the next spike.
	cfg := testConfig()
	history := []Recommendation{{Time: t0.Add(-time.Minute), Replicas: 1}}

	out := Recommend(Input{
		Now: t0, Current: 2, ReadyReplicas: 2,
		Observation: measured(16), Config: cfg, History: history,
	})
	if out.Replicas != 8 {
		t.Fatalf("replicas = %d, want 8: scale-up stabilization is zero by default", out.Replicas)
	}
}

func TestScaleUpStabilizationRequiresSustainedLoad(t *testing.T) {
	t.Parallel()

	cfg := testConfig(func(c *Config) { c.ScaleUpStabilization = 2 * time.Minute })

	// A single spike against a recent history of low recommendations must not
	// add capacity on its own.
	history := []Recommendation{
		{Time: t0.Add(-90 * time.Second), Replicas: 2},
		{Time: t0.Add(-60 * time.Second), Replicas: 2},
	}

	out := Recommend(Input{
		Now: t0, Current: 2, ReadyReplicas: 2,
		Observation: measured(16), Config: cfg, History: history,
	})
	if out.Replicas != 2 {
		t.Fatalf("replicas = %d, want 2: one spike must not scale up through the window", out.Replicas)
	}

	// Sustained load does.
	sustained := []Recommendation{
		{Time: t0.Add(-90 * time.Second), Replicas: 8},
		{Time: t0.Add(-60 * time.Second), Replicas: 8},
	}
	out = Recommend(Input{
		Now: t0, Current: 2, ReadyReplicas: 2,
		Observation: measured(16), Config: cfg, History: sustained,
	})
	if out.Replicas != 8 {
		t.Fatalf("replicas = %d, want 8 once the load has been high for the whole window", out.Replicas)
	}
}

func TestStabilizationNeverReversesADecision(t *testing.T) {
	t.Parallel()

	// The subtle one. A scale-UP decision must never come out as a scale-DOWN
	// because the window's minimum happens to sit below the current count —
	// that would shrink the fleet at the exact moment the metric asks for more.
	cfg := testConfig(func(c *Config) { c.ScaleUpStabilization = 5 * time.Minute })
	history := []Recommendation{
		{Time: t0.Add(-4 * time.Minute), Replicas: 1},
		{Time: t0.Add(-2 * time.Minute), Replicas: 1},
	}

	out := Recommend(Input{
		Now: t0, Current: 6, ReadyReplicas: 6,
		Observation: measured(18), Config: cfg, History: history,
	})
	if out.Replicas < 6 {
		t.Fatalf("replicas = %d: an up decision was turned into a scale-down by stabilization",
			out.Replicas)
	}

	// And the mirror image: a scale-DOWN must not become a scale-up.
	cfgDown := testConfig(func(c *Config) { c.ScaleDownStabilization = 5 * time.Minute })
	highHistory := []Recommendation{
		{Time: t0.Add(-2 * time.Minute), Replicas: 10},
	}
	out = Recommend(Input{
		Now: t0, Current: 4, ReadyReplicas: 4,
		Observation: measured(1), Config: cfgDown, History: highHistory,
	})
	if out.Replicas > 4 {
		t.Fatalf("replicas = %d: a down decision was turned into a scale-up by stabilization",
			out.Replicas)
	}
}

func TestHistoryIsPrunedAndBounded(t *testing.T) {
	t.Parallel()

	// Unbounded history in a status field is an etcd leak with a slow fuse: the
	// object grows on every poll and every watcher re-reads it.
	cfg := testConfig()

	history := make([]Recommendation, 0, 200)
	for i := range 200 {
		history = append(history, Recommendation{
			Time:     t0.Add(-time.Duration(i) * time.Second),
			Replicas: 4,
		})
	}

	out := Recommend(Input{
		Now: t0, Current: 4, ReadyReplicas: 4,
		Observation: measured(8), Config: cfg, History: history,
	})

	if len(out.History) > maxHistory {
		t.Fatalf("history has %d entries, want at most %d", len(out.History), maxHistory)
	}

	// Entries older than the longest window are gone.
	cutoff := t0.Add(-cfg.ScaleDownStabilization)
	for _, r := range out.History {
		if r.Time.Before(cutoff) {
			t.Fatalf("history retained an entry from %v, older than the %v window",
				r.Time, cfg.ScaleDownStabilization)
		}
	}
}

func TestHistoryDropsFutureEntries(t *testing.T) {
	t.Parallel()

	// A future-stamped entry can only come from a clock that moved backwards,
	// and keeping one would pin the stabilization window open until real time
	// caught up.
	out := Recommend(Input{
		Now: t0, Current: 4, ReadyReplicas: 4,
		Observation: measured(8), Config: testConfig(),
		History: []Recommendation{
			{Time: t0.Add(time.Hour), Replicas: 99},
			{Time: t0.Add(-time.Minute), Replicas: 4},
		},
	})

	for _, r := range out.History {
		if r.Time.After(t0) {
			t.Fatalf("history retained a future entry stamped %v", r.Time)
		}
	}
}

func TestRecommendIsDeterministic(t *testing.T) {
	t.Parallel()

	// The reconcile loop calls this on every pass; identical input must produce
	// identical output or the fleet churns for no reason.
	in := Input{
		Now: t0, Current: 4, ReadyReplicas: 3,
		Observation: measured(11), Config: testConfig(),
		History: []Recommendation{
			{Time: t0.Add(-time.Minute), Replicas: 5},
			{Time: t0.Add(-30 * time.Second), Replicas: 6},
		},
	}

	first := Recommend(in)
	for i := range 100 {
		got := Recommend(in)
		if got.Replicas != first.Replicas || got.Reason != first.Reason ||
			got.Raw != first.Raw || len(got.History) != len(first.History) {
			t.Fatalf("iteration %d differs:\nfirst: %+v\ngot:   %+v", i, first, got)
		}
	}
}

// TestRecommendConvergesRatherThanOscillating is the property a fleet's
// stability actually depends on.
//
// A closed loop is simulated: the recommendation becomes the replica count, and
// the per-pod load is recomputed from a fixed total demand. Without a tolerance
// band this oscillates forever; with one it must settle and stay settled.
func TestRecommendConvergesRatherThanOscillating(t *testing.T) {
	t.Parallel()

	cfg := testConfig(func(c *Config) { c.ScaleDownStabilization = 0 })

	const totalDemand = 13.0 // 6.5 pods' worth at a target of 2

	replicas := int32(1)
	var history []Recommendation
	now := t0

	var lastFive []int32
	for range 40 {
		out := Recommend(Input{
			Now: now, Current: replicas, ReadyReplicas: replicas,
			Observation: measured(totalDemand), Config: cfg, History: history,
		})
		replicas = out.Replicas
		history = out.History
		now = now.Add(15 * time.Second)

		lastFive = append(lastFive, replicas)
		if len(lastFive) > 5 {
			lastFive = lastFive[1:]
		}
	}

	for _, r := range lastFive {
		if r != lastFive[0] {
			t.Fatalf("the fleet never settled; the last five recommendations were %v — "+
				"this is what a missing tolerance band looks like", lastFive)
		}
	}
	if replicas < 6 || replicas > 8 {
		t.Fatalf("settled at %d replicas for a demand of %.0f at target 2; "+
			"expected somewhere around 7", replicas, totalDemand)
	}
}

func TestRecommendNeverExceedsBounds(t *testing.T) {
	t.Parallel()

	// Exhaustive over a realistic input space rather than sampled: the awkward
	// combinations are all at the edges.
	cfg := testConfig(func(c *Config) { c.MinReplicas = 2; c.MaxReplicas = 9 })

	for current := int32(0); current <= 12; current++ {
		for ready := int32(0); ready <= 12; ready++ {
			for _, value := range []float64{0, 0.5, 2, 20, 200, math.MaxFloat64} {
				out := Recommend(Input{
					Now: t0, Current: current, ReadyReplicas: ready,
					Observation: measured(value), Config: cfg,
				})
				if out.Replicas < cfg.MinReplicas || out.Replicas > cfg.MaxReplicas {
					t.Fatalf("current=%d ready=%d value=%v: replicas = %d, outside [%d, %d]",
						current, ready, value, out.Replicas, cfg.MinReplicas, cfg.MaxReplicas)
				}
			}
		}
	}
}

// TestRecommendationIsStableWhileTheFleetStartsUp is the property the
// ready-count reduction buys.
//
// Six pods were asked for, they come up one at a time, and the queued demand
// stays constant throughout. The recommendation must not climb on every poll
// just because the pods that ARE ready look overloaded — that is how an
// autoscaler runs away from itself during a cold start, and for an engine with
// a multi-minute weight load the runaway has plenty of time to reach the
// ceiling.
func TestRecommendationIsStableWhileTheFleetStartsUp(t *testing.T) {
	t.Parallel()

	cfg := testConfig(func(c *Config) { c.MaxReplicas = 20 })
	const queued = 12.0 // wants 6 pods at a target of 2

	var first int32
	for ready := int32(1); ready <= 6; ready++ {
		out := Recommend(Input{
			Now: t0, Current: 6, ReadyReplicas: ready,
			Observation: measured(queued), Config: cfg,
		})
		if ready == 1 {
			first = out.Raw
			continue
		}
		if out.Raw != first {
			t.Fatalf("ready=%d: raw recommendation moved to %d from %d while demand was constant",
				ready, out.Raw, first)
		}
	}
	if first != 6 {
		t.Fatalf("raw = %d for %.0f queued at a target of 2, want 6", first, queued)
	}
}

func TestRecommendHandlesNonFiniteMeasurements(t *testing.T) {
	t.Parallel()

	// A NaN or an infinity reaching here means the provider layer let one
	// through. int32(math.Ceil(NaN)) is undefined behaviour territory, and the
	// resulting replica count would be arbitrary.
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		out := Recommend(Input{
			Now: t0, Current: 4, ReadyReplicas: 4,
			Observation: measured(value), Config: testConfig(),
		})
		if out.Replicas < 1 || out.Replicas > 10 {
			t.Fatalf("value %v produced %d replicas, outside the configured bounds",
				value, out.Replicas)
		}
	}
}

func TestClampToleratesAnInvertedRange(t *testing.T) {
	t.Parallel()

	// CEL rejects maxReplicas < minReplicas, so this is defence in depth for an
	// object written before that rule existed. Preferring the floor keeps
	// capacity rather than removing it.
	out := Recommend(Input{
		Now: t0, Current: 5, ReadyReplicas: 5,
		Observation: measured(50),
		Config:      Config{MinReplicas: 4, MaxReplicas: 2, Target: 2, Tolerance: 0.1},
	})
	if out.Replicas != 4 {
		t.Fatalf("replicas = %d, want the floor of 4", out.Replicas)
	}
}
