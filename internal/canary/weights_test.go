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
)

// TestSplitInvariants exhausts the whole realistic input space.
//
// Not a sample of it — all of it. total in [1,20] crossed with weight in
// [0,100] is 2100 cases, which runs in well under a millisecond, and the
// invariants below are the ones a rollout's correctness rests on. Sampling
// would leave exactly the awkward combinations untested: 1 replica, 99%, and
// the boundaries where rounding changes direction.
func TestSplitInvariants(t *testing.T) {
	t.Parallel()

	for total := int32(1); total <= 20; total++ {
		for weight := int32(0); weight <= 100; weight++ {
			primary, canary := Split(total, weight)

			if primary+canary != total {
				t.Fatalf("total=%d weight=%d: split %d+%d != %d — a rollout must never over- or "+
					"under-provision", total, weight, primary, canary, total)
			}
			if primary < 0 || canary < 0 {
				t.Fatalf("total=%d weight=%d: negative split %d/%d", total, weight, primary, canary)
			}

			if weight > 0 && weight < 100 && total > 1 && canary < 1 {
				t.Fatalf("total=%d weight=%d: canary got %d pods; a canary with no pods receives no "+
					"traffic, and analysis over no traffic is the failure this design exists to avoid",
					total, weight, canary)
			}
			if weight < 100 && total > 1 && primary < 1 {
				t.Fatalf("total=%d weight=%d: primary got %d pods; there must always be something "+
					"already serving to roll back to", total, weight, primary)
			}
			if weight == 0 && canary != 0 {
				t.Fatalf("total=%d weight=0: canary got %d pods", total, canary)
			}
			if weight == 100 && primary != 0 {
				t.Fatalf("total=%d weight=100: primary kept %d pods", total, primary)
			}
		}
	}
}

func TestSplitRoundsUp(t *testing.T) {
	t.Parallel()

	// Rounding up is deliberate: a canary receiving MORE traffic than requested
	// is judged on more evidence, whereas one receiving less can pass on almost
	// none.
	cases := []struct {
		total, weight   int32
		primary, canary int32
	}{
		{total: 10, weight: 20, primary: 8, canary: 2},
		{total: 10, weight: 50, primary: 5, canary: 5},
		{total: 3, weight: 20, primary: 2, canary: 1}, // 0.6 pods -> 1
		{total: 5, weight: 10, primary: 4, canary: 1}, // 0.5 pods -> 1
		{total: 4, weight: 25, primary: 3, canary: 1}, // exact
		{total: 7, weight: 30, primary: 4, canary: 3}, // 2.1 pods -> 3
		{total: 2, weight: 50, primary: 1, canary: 1}, // exact
		// One pod cannot be split, and the primary keeps it: handing it to the
		// canary would put all of production on an unproven revision while
		// calling it a 50% exposure.
		{total: 1, weight: 50, primary: 1, canary: 0},
		{total: 10, weight: 95, primary: 1, canary: 9}, // primary keeps one
	}

	for _, tc := range cases {
		primary, canary := Split(tc.total, tc.weight)
		if primary != tc.primary || canary != tc.canary {
			t.Errorf("Split(%d, %d) = (%d, %d), want (%d, %d)",
				tc.total, tc.weight, primary, canary, tc.primary, tc.canary)
		}
	}
}

func TestSplitZeroTotal(t *testing.T) {
	t.Parallel()

	// Scaled to zero is a state a user asked for, not a failure. It must not
	// produce a negative primary or a phantom canary.
	primary, canary := Split(0, 50)
	if primary != 0 || canary != 0 {
		t.Fatalf("Split(0, 50) = (%d, %d), want (0, 0)", primary, canary)
	}
}

func TestRealizedWeightIsHonest(t *testing.T) {
	t.Parallel()

	// The quantisation the operator reports rather than hides. Publishing only
	// the requested number would let someone believe the blast radius is 20%
	// when a third of production is on the new version.
	cases := []struct {
		total, requested, realized int32
	}{
		{total: 3, requested: 20, realized: 33},
		{total: 10, requested: 20, realized: 20},
		{total: 2, requested: 10, realized: 50},
		{total: 5, requested: 30, realized: 40},
		{total: 4, requested: 50, realized: 50},
	}

	for _, tc := range cases {
		primary, canary := Split(tc.total, tc.requested)
		got := RealizedWeight(primary, canary)
		if got != tc.realized {
			t.Errorf("total=%d requested=%d: realised %d%%, want %d%%",
				tc.total, tc.requested, got, tc.realized)
		}
	}
}

func TestRealizedWeightBounds(t *testing.T) {
	t.Parallel()

	for total := int32(1); total <= 20; total++ {
		for weight := int32(0); weight <= 100; weight++ {
			p, c := Split(total, weight)
			w := RealizedWeight(p, c)
			if w < 0 || w > 100 {
				t.Fatalf("total=%d weight=%d: realised %d%% is outside [0,100]", total, weight, w)
			}
		}
	}
	if got := RealizedWeight(0, 0); got != 0 {
		t.Errorf("RealizedWeight(0,0) = %d, want 0", got)
	}
}

func TestIsQuantizedTolerance(t *testing.T) {
	t.Parallel()

	// A one-point tolerance keeps a rounding artefact from producing a warning
	// event on every single reconcile.
	if IsQuantized(20, 21) {
		t.Error("a one-point difference should not count as quantised")
	}
	if IsQuantized(20, 20) {
		t.Error("an exact match should not count as quantised")
	}
	if !IsQuantized(20, 33) {
		t.Error("20 vs 33 is a real quantisation and must be reported")
	}
	if !IsQuantized(50, 10) {
		t.Error("quantisation in the other direction must be reported too")
	}
}
