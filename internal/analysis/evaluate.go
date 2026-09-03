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

package analysis

import (
	"fmt"
	"math"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Observation is one metric's raw result, before it is judged.
type Observation struct {
	// Sample is what the backend returned.
	Sample Sample

	// Err is a transport-level failure, if any.
	Err error
}

// Evaluate turns one observation into a verdict.
//
// # This function is where the "healthy because empty" bug lives
//
// Every branch below exists because some real canary controller shipped without
// it and promoted a broken release. The order is not arbitrary — each check
// eliminates a class of non-number before the next one assumes it has a number:
//
//  1. A transport error is an ERROR. The backend said nothing, so nothing is
//     known about the canary. Crucially this is not a Fail: a Prometheus outage
//     must never roll back a production deployment.
//
//  2. An empty result is INCONCLUSIVE, not zero. `sum(rate(errors[1m]))` over a
//     series that does not exist returns nothing, and reading that as "zero
//     errors" is exactly how a canary with a crashed exporter passes every
//     check.
//
//  3. More than one series is an ERROR. The query did not aggregate; picking
//     the first sample would silently measure an arbitrary pod. Reporting it
//     loudly is the only way the author of a raw query ever finds out.
//
//  4. NaN is INCONCLUSIVE. histogram_quantile over empty buckets returns NaN,
//     and NaN compares FALSE against every threshold — so an unguarded
//     `value > max` reports Pass. This branch is the difference between a gate
//     and a decoration.
//
//  5. ±Inf is INCONCLUSIVE. It means a ratio's denominator collapsed despite
//     the clamp, which says the comparison could not be made rather than that
//     it came out badly.
//
//  6. Only now is the value compared against the range, inclusively at both
//     bounds. Inclusive because a threshold written as "at most 1.5" should
//     accept exactly 1.5; an exclusive bound makes the boundary case depend on
//     floating-point representation, which is not a property anyone intends to
//     gate a release on.
func Evaluate(m inferencev1alpha1.AnalysisMetric, obs Observation) inferencev1alpha1.MetricCheck {
	check := inferencev1alpha1.MetricCheck{
		Name:      m.Name,
		Threshold: renderRange(m.ThresholdRange),
	}

	if obs.Err != nil {
		check.Verdict = inferencev1alpha1.VerdictError
		check.Message = "querying the metric provider failed: " + obs.Err.Error()
		return check
	}

	switch {
	case obs.Sample.Count == 0:
		check.Verdict = inferencev1alpha1.VerdictInconclusive
		check.Message = "the query matched no series; this is not a measurement of zero, " +
			"it is the absence of a measurement"
		return check

	case obs.Sample.Count > 1:
		check.Verdict = inferencev1alpha1.VerdictError
		check.Message = fmt.Sprintf(
			"the query returned %d series; it must aggregate to exactly one "+
				"(a missing sum() or sum by (le)), because choosing among them would measure an arbitrary pod",
			obs.Sample.Count)
		return check
	}

	v := obs.Sample.Value
	check.Value = renderValue(v)

	switch {
	case math.IsNaN(v):
		check.Verdict = inferencev1alpha1.VerdictInconclusive
		check.Message = "the query returned " + LiteralNaN + ", which usually means the histogram had no observations " +
			"in the analysis window"
		return check

	case math.IsInf(v, 0):
		check.Verdict = inferencev1alpha1.VerdictInconclusive
		check.Message = "the query returned an infinity, which means a ratio's denominator was empty; " +
			"the comparison could not be made"
		return check
	}

	if minQ := m.ThresholdRange.Min; minQ != nil {
		if v < quantityFloat(minQ) {
			check.Verdict = inferencev1alpha1.VerdictFail
			check.Message = fmt.Sprintf("%s is below the minimum of %s", check.Value, minQ.String())
			return check
		}
	}
	if maxQ := m.ThresholdRange.Max; maxQ != nil {
		if v > quantityFloat(maxQ) {
			check.Verdict = inferencev1alpha1.VerdictFail
			check.Message = fmt.Sprintf("%s exceeds the maximum of %s", check.Value, maxQ.String())
			return check
		}
	}

	check.Verdict = inferencev1alpha1.VerdictPass
	return check
}

// Inconclusive builds a check that could not be judged, for the round-level
// gates that short-circuit before any metric is queried.
func Inconclusive(m inferencev1alpha1.AnalysisMetric, message string) inferencev1alpha1.MetricCheck {
	return inferencev1alpha1.MetricCheck{
		Name:      m.Name,
		Verdict:   inferencev1alpha1.VerdictInconclusive,
		Threshold: renderRange(m.ThresholdRange),
		Message:   message,
	}
}

// Errored builds a check that failed at the provider, for the same reason.
func Errored(m inferencev1alpha1.AnalysisMetric, message string) inferencev1alpha1.MetricCheck {
	return inferencev1alpha1.MetricCheck{
		Name:      m.Name,
		Verdict:   inferencev1alpha1.VerdictError,
		Threshold: renderRange(m.ThresholdRange),
		Message:   message,
	}
}

// quantityFloat converts a threshold Quantity to a float for comparison.
//
// Via MilliValue rather than AsApproximateFloat64. Thresholds in this API are
// written as things like "1500m" and "0.05", which are exact in milli-units and
// inexact in binary floating point; going through MilliValue means a threshold
// of "1500m" compares as exactly 1.5 rather than as 1.4999999999999998, and a
// value sitting precisely on the boundary lands on the intended side of it.
func quantityFloat(q *resource.Quantity) float64 {
	return float64(q.MilliValue()) / 1000
}

// renderValue formats an observed value for status.
//
// Six significant digits: enough to distinguish two latency percentiles that
// differ in the low milliseconds, short enough that a status field stays
// readable. Note this deliberately renders NaN and Inf as themselves rather
// than blanking them — a status that says "NaN" tells the reader exactly what
// happened, whereas an empty field looks like a bug in the operator.
func renderValue(v float64) string {
	if math.IsNaN(v) {
		return LiteralNaN
	}
	if math.IsInf(v, 1) {
		return LiteralPosInf
	}
	if math.IsInf(v, -1) {
		return LiteralNegInf
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// renderRange restates a threshold for status, so a status read weeks later
// still explains itself without the spec beside it.
func renderRange(r inferencev1alpha1.ThresholdRange) string {
	switch {
	case r.Min != nil && r.Max != nil:
		return "[" + r.Min.String() + ", " + r.Max.String() + "]"
	case r.Min != nil:
		return ">= " + r.Min.String()
	case r.Max != nil:
		return "<= " + r.Max.String()
	default:
		// CEL rejects this at admission. Reaching it means an object was
		// written before the validation existed, and saying so is better than
		// rendering an empty string that reads like "no threshold applied".
		return "unbounded"
	}
}
