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

package metrics

import (
	"slices"
	"strconv"
)

// Histogram bucket boundaries.
//
// These live here, beside the metric names, because two different binaries need
// to agree on them and the consequence of disagreeing is silent.
//
//   - The SHIM uses them to define its histograms.
//   - The SLO rule generator uses them to pick the `le` label for a latency
//     objective.
//
// A latency SLI is "the fraction of requests faster than X", and in PromQL that
// is a ratio of the bucket at `le="X"` over the total count. Prometheus matches
// `le` as an exact STRING: a rule asking for `le="0.5"` against a histogram
// whose nearest boundaries are 0.4 and 0.6 does not approximate, and does not
// error — it returns an empty vector. The recording rule then produces nothing,
// the burn-rate alert compares against no data, and the alert never fires. An
// SLO that can never alert is worse than no SLO, because it is believed.
//
// Sharing the definition means the threshold can be SNAPPED to a real boundary
// before it reaches a rule, and the snapping can be reported.
//
// # Why not the client library's defaults
//
// prometheus.DefBuckets spans 5 ms to 10 s in a shape tuned for web request
// handlers. That puts almost every boundary below where CPU inference actually
// lives and leaves a real workload's p95 in the +Inf bucket, where
// histogram_quantile can only interpolate — reporting a number that moves when
// nothing changed. Bucket boundaries are also the resolution limit of every
// percentile computed from them, and canary analysis compares two p95 values
// and asks whether their ratio crossed a threshold: if both land in the same
// wide bucket, the answer is "no change" regardless of the truth.
var (
	// TTFTBuckets span 10 ms (a warm stub) to 30 s (a cold CPU model under
	// load), with the tightest spacing between 100 ms and 2 s, which is where a
	// 0.6B model on a laptop CPU actually sits.
	TTFTBuckets = []float64{
		0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.4, 0.6, 0.8,
		1, 1.5, 2, 3, 5, 7.5, 10, 15, 20, 30,
	}

	// DurationBuckets extend further: a full generation is TTFT plus one
	// inter-token interval per output token, so it is longer by construction
	// and its useful range is wider.
	DurationBuckets = []float64{
		0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 7.5, 10, 15, 20, 30, 45, 60, 120,
	}

	// TPOTBuckets are per-token, so they live two orders of magnitude below the
	// others: a healthy CPU decode is single-digit to low-double-digit
	// milliseconds per token.
	TPOTBuckets = []float64{
		0.001, 0.0025, 0.005, 0.01, 0.02, 0.035, 0.05, 0.075,
		0.1, 0.15, 0.25, 0.5, 1, 2.5,
	}
)

// SnapToBucket returns the smallest bucket boundary at or above threshold, and
// whether it differs from what was asked for.
//
// Rounding UP rather than to the nearest is deliberate. The SLI reads "the
// fraction of requests faster than the boundary", so snapping up makes the
// objective slightly EASIER to meet than the number written in the spec.
// Snapping down would make it harder — silently holding a service to a target
// stricter than its owner declared, which is how an SLO ends up burning budget
// for a latency nobody considered a violation.
//
// A threshold above the last boundary snaps to that boundary rather than to
// +Inf. `le="+Inf"` is a valid selector but it selects every request, so the
// ratio would be a constant 1.0 and the SLI would report perfect health
// forever.
func SnapToBucket(threshold float64, buckets []float64) (snapped float64, changed bool) {
	if len(buckets) == 0 {
		return threshold, false
	}

	idx, exact := slices.BinarySearch(buckets, threshold)
	switch {
	case exact:
		return threshold, false
	case idx >= len(buckets):
		return buckets[len(buckets)-1], true
	default:
		return buckets[idx], true
	}
}

// FormatBucket renders a bucket boundary exactly as the Prometheus client
// library writes it into an `le` label.
//
// This has to match byte for byte or the selector matches nothing. The client
// library formats bucket boundaries with strconv.FormatFloat(f, 'f', -1, 64) —
// the shortest representation that round-trips — so "0.25" stays "0.25" and
// "1" stays "1" rather than becoming "1.0". Constructing the string any other
// way is how a rule ends up selecting a bucket that does not exist.
func FormatBucket(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
