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

// Split divides a total replica count between the primary and canary variants
// so that the canary receives approximately weight percent of the traffic.
//
// # The replica-ownership contract
//
// There is exactly one place that decides HOW MANY pods exist and exactly one
// place that decides how they are DIVIDED. The autoscaler produces a desired
// total; this function distributes it. Two functions, one input, no fight — a
// canary controller and an autoscaler that each believe they own the replica
// count is the classic way for a rollout to oscillate forever, and it is the
// bug Argo Rollouts spent several releases resolving.
//
// # Quantisation is unavoidable and is reported, not hidden
//
// Traffic is split by pod count, so the achievable weights are the multiples of
// 1/total. At 3 replicas the nearest expressible value to 20% is 33%. Rounding
// is deliberately UP — a canary that receives more traffic than requested is
// measured on more evidence, whereas one that receives less can pass on almost
// none — and the realised figure is published as status.canary.currentWeight
// beside the requested one.
//
// # The invariants
//
// For any total >= 1 and any weight in [0, 100]:
//
//   - primary + canary == total, exactly. Never over-provisioning is what keeps
//     a canary from doubling the cost of a rollout, and never under-provisioning
//     is what keeps it from being an outage.
//   - weight > 0 AND total > 1 implies canary >= 1. A canary with no pods
//     receives no traffic, and analysis over no traffic is the "healthy because
//     empty" failure this whole design exists to avoid.
//   - weight < 100 implies primary >= 1. There must always be something already
//     serving to roll back to.
//
// The two cannot both hold at a total of ONE, and the second wins: the single
// pod stays with the primary. Handing it to the canary would put 100% of
// production on an unproven revision while calling it a 20% exposure, which is
// worse than not canarying at all. The state machine detects this case up front
// and declines to start, with a message saying why — see begin() in state.go.
//
// These are enforced by a property test over the full input space, not by
// inspection.
func Split(total, weight int32) (primary, canary int32) {
	if total <= 0 {
		return 0, 0
	}
	switch {
	case weight <= 0:
		return total, 0
	case weight >= 100:
		return 0, total
	}

	// Round up: see the note above on why more exposure is the safer error.
	canary = (total*weight + 99) / 100

	canary = max(canary, 1)

	// Leave the primary at least one pod, so there is always a running
	// fallback. At a total of one this drives the canary back to zero, which is
	// the documented behaviour above.
	if canary >= total {
		canary = total - 1
	}
	canary = max(canary, 0)

	return total - canary, canary
}

// RealizedWeight is the traffic share a split actually achieves, rounded to the
// nearest whole percent.
//
// Reported as status.canary.currentWeight next to the requested
// desiredWeight. Publishing only the request would let an operator believe the
// blast radius is 20% when it is a third of production, and would make the
// analysis appear to be judging an exposure it is not.
func RealizedWeight(primary, canary int32) int32 {
	total := primary + canary
	if total <= 0 {
		return 0
	}
	return int32((int64(canary)*100 + int64(total)/2) / int64(total))
}

// IsQuantized reports whether the achievable weight differs from the requested
// one by more than a percentage point.
//
// The one-point tolerance keeps a rounding artefact from producing an event on
// every reconcile: at 7 replicas a requested 43% realises as 43% but arrives
// there through integer arithmetic that can land a fraction either side.
func IsQuantized(desired, realized int32) bool {
	d := desired - realized
	if d < 0 {
		d = -d
	}
	return d > 1
}

// CanaryReplicas resolves how many pods the canary should run, honouring an
// explicit override.
//
// A fixed canary size decoupled from the traffic weight matters more for
// inference than for a stateless web service: a model server has a long,
// expensive warm-up, so a canary resized at every step spends the first
// analysis window of each one loading a model rather than serving requests —
// and the latency it reports during that window is the warm-up, not the
// revision under test.
//
// The override is still clamped to leave the primary at least one pod. A spec
// asking for more canary replicas than the total would otherwise silently
// delete the fallback the rollback depends on.
func CanaryReplicas(total, weight int32, override *int32) (primary, canary int32) {
	if override == nil {
		return Split(total, weight)
	}

	canary = *override
	canary = max(canary, 0)

	if canary >= total {
		canary = max(total-1, 0)
	}
	return total - canary, canary
}
