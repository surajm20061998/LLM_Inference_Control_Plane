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
	"strings"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

// clampFloor is the denominator guard applied to every ratio this file builds.
//
// # Why every ratio needs one
//
// PromQL division by zero yields NaN when the numerator is also zero, and +Inf
// otherwise. Both render as "No data" on a dashboard and both are silently
// wrong on a rollout gate: NaN compares false against every threshold, so an
// unguarded `value > max` check reports PASS for a canary that served nothing
// at all. That is the "healthy because empty" failure, and it is the single
// most common defect in a homegrown canary controller.
//
// clamp_min turns the pathological case into an honest, enormous number — a
// zero-traffic error rate becomes 0/1e-9 = 0, and a nonzero numerator over no
// denominator becomes a value so large that no threshold accepts it. Combined
// with the separate minRequestRate gate, which catches the case BEFORE it gets
// here, a canary can no longer pass by receiving no traffic.
const clampFloor = "1e-9"

// Query renders the PromQL for one metric and one variant.
//
// Built-in queries are written once, here, correctly. Raw PromQL in user hands
// is a NaN factory — a forgotten rate(), an unclamped denominator, a window
// shorter than the scrape interval — and each of those produces a number that
// looks plausible and gates a production rollout on nothing. The Query escape
// hatch on AnalysisMetric remains for cases the built-ins do not cover; it is
// substituted, not validated, and that trade is stated in its own field doc.
func Query(m inferencev1alpha1.AnalysisMetric, qc QueryContext) (string, error) {
	if m.Query != "" {
		return substitute(m.Query, qc), nil
	}
	if m.Builtin == nil {
		return "", fmt.Errorf("metric %q sets neither builtin nor query", m.Name)
	}
	return builtinQuery(*m.Builtin, qc)
}

// builtinQuery renders one of the named built-ins.
func builtinQuery(b inferencev1alpha1.BuiltinMetric, qc QueryContext) (string, error) {
	sel := selector(qc)

	switch b {
	case inferencev1alpha1.MetricTTFTP95:
		return histogramQuantile(0.95, llmcpmetrics.TTFTSeconds, sel, qc.Window), nil

	case inferencev1alpha1.MetricTTFTP99:
		return histogramQuantile(0.99, llmcpmetrics.TTFTSeconds, sel, qc.Window), nil

	case inferencev1alpha1.MetricRequestDurationP95:
		return histogramQuantile(0.95, llmcpmetrics.RequestDurationSeconds, sel, qc.Window), nil

	case inferencev1alpha1.MetricErrorRate:
		return errorRateQuery(qc), nil

	case inferencev1alpha1.MetricSuccessRate:
		// Written as 1 minus the error rate rather than as its own ratio of
		// 2xx over total. The two are not equivalent: counting successes as
		// "status 2xx" would classify a 4xx — a client's malformed prompt — as
		// a server failure, and a canary would then be rolled back because
		// somebody sent it bad JSON.
		return "1 - (" + errorRateQuery(qc) + ")", nil

	case inferencev1alpha1.MetricQueueDepth:
		// avg_over_time, not the instantaneous gauge. Queue depth is spiky by
		// nature — it is the difference between arrivals and a fixed number of
		// decode slots — and an instant read samples whichever microsecond the
		// scrape landed on.
		return fmt.Sprintf("avg(avg_over_time(%s{%s}[%s]))",
			llmcpmetrics.QueueDepth, sel, qc.Window), nil

	case inferencev1alpha1.MetricOutputTokenRate:
		return fmt.Sprintf("sum(rate(%s{%s}[%s]))",
			llmcpmetrics.OutputTokensTotal, sel, qc.Window), nil

	default:
		return "", fmt.Errorf("unknown builtin metric %q", b)
	}
}

// AutoscalingQuery renders the fleet-wide query the built-in autoscaler tracks.
//
// # Why this omits the variant label
//
// Every other query in this file pins `variant`, because comparing a canary
// against its primary is the entire point of analysis. This one deliberately
// does not.
//
// An autoscaler decides how much capacity EXISTS; a rollout decides how that
// capacity is split. Measuring one variant would size the whole fleet from a
// fraction of its load — and during a canary that fraction changes at every
// step of the weight ladder, so the autoscaler would end up reacting to the
// rollout's progress rather than to demand. The two controllers would then be
// coupled through a metric, which is precisely the coupling the
// Recommend/Split separation exists to prevent.
//
// The aggregation is a SUM over an average-over-time, in that order. The
// average smooths each pod's spiky gauge over the window; the sum then adds up
// what the whole fleet is carrying. Averaging the sum instead would be the same
// number, but summing an instantaneous gauge would not: it would sample
// whichever microsecond each scrape happened to land on.
func AutoscalingQuery(metric inferencev1alpha1.AutoscalingMetric, qc QueryContext) (string, error) {
	var name string
	switch metric {
	case inferencev1alpha1.AutoscalingQueueDepth:
		name = llmcpmetrics.QueueDepth
	case inferencev1alpha1.AutoscalingConcurrency:
		name = llmcpmetrics.RequestsInFlight
	default:
		return "", fmt.Errorf("unknown autoscaling metric %q", metric)
	}

	return fmt.Sprintf("sum(avg_over_time(%s{%s}[%s]))",
		name, fleetSelector(qc), qc.Window), nil
}

// fleetSelector pins the model but not the variant.
func fleetSelector(qc QueryContext) string {
	return fmt.Sprintf(`%s=%q`, llmcpmetrics.LabelModel, qc.Model)
}

// RequestRateQuery is the traffic gate's query: requests per second for one
// variant.
//
// It is not one of the user-selectable built-ins because it is not a threshold
// check — it is the precondition every other check depends on. A canary that
// received no traffic has not been tested, and running the latency comparison
// against it produces a confident answer about nothing.
func RequestRateQuery(qc QueryContext) string {
	return fmt.Sprintf("sum(rate(%s{%s}[%s]))",
		llmcpmetrics.RequestsTotal, selector(qc), qc.Window)
}

// RatioQuery divides a canary query by the same query against the primary.
//
// # Why a ratio is usually the better gate
//
// An absolute latency threshold has to be re-tuned for every model, every node
// type and every prompt mix. One that is stale fires on a busy afternoon rather
// than on a bad release, and after that happens twice people stop trusting the
// gate. A ratio asks the only question a rollout actually needs answered — is
// the new version worse than the one it is replacing? — and answers it under
// whatever conditions happen to hold right now, including a node that is having
// a bad day for reasons unrelated to the release.
//
// The denominator is clamped for the reason given on clampFloor: a primary that
// reported nothing must not make the canary look infinitely bad.
func RatioQuery(canaryQuery, primaryQuery string) string {
	return "(" + canaryQuery + ") / clamp_min(" + primaryQuery + ", " + clampFloor + ")"
}

// histogramQuantile renders a percentile over a histogram's buckets.
//
// `sum by (le)` is the part that is easy to get wrong and expensive to get
// wrong. Without it the expression produces one series per scraped pod, the
// provider reports more than one sample, and the check becomes an Error — which
// is the good outcome. The bad outcome is the variant of this query that
// aggregates over the wrong labels and silently averages a canary's pods
// together with its primary's, producing a percentile that always sits between
// the two and can therefore never cross a threshold.
func histogramQuantile(q float64, metric, sel, window string) string {
	return fmt.Sprintf("histogram_quantile(%g, sum by (le) (rate(%s_bucket{%s}[%s])))",
		q, metric, sel, window)
}

// errorRateQuery renders the 5xx share of requests as a fraction in [0,1].
//
// 5xx only, deliberately. A 4xx is the client's fault — a malformed prompt, a
// model name that does not exist, a context window overflow — and counting one
// against the server would let a single misbehaving caller roll back a
// perfectly good release.
func errorRateQuery(qc QueryContext) string {
	base := selector(qc)
	errSel := base + `,` + llmcpmetrics.LabelCode + `=~"5.."`

	return fmt.Sprintf("sum(rate(%s{%s}[%s])) / clamp_min(sum(rate(%s{%s}[%s])), %s)",
		llmcpmetrics.RequestsTotal, errSel, qc.Window,
		llmcpmetrics.RequestsTotal, base, qc.Window,
		clampFloor)
}

// selector renders the label matcher shared by every built-in.
//
// Both labels are always present. Omitting `variant` would merge the canary's
// series with the primary's, which for a comparison between the two is not an
// imprecision but an inversion — the canary would be measured partly against
// itself, and a regression would be diluted by exactly the traffic it was
// supposed to be detected in.
func selector(qc QueryContext) string {
	return fmt.Sprintf(`%s=%q,%s=%q`,
		llmcpmetrics.LabelModel, qc.Model,
		llmcpmetrics.LabelVariant, qc.Variant)
}

// substitute expands the template variables allowed in a raw query.
//
// A deliberately tiny substitution rather than text/template. The full template
// engine would let a query reference arbitrary fields of whatever struct it was
// handed, which turns a spec field into a read primitive over the controller's
// internal state — and it would fail at execution time, mid-rollout, on a typo
// that a plain string replacement simply leaves in place for Prometheus to
// reject clearly.
func substitute(query string, qc QueryContext) string {
	return strings.NewReplacer(
		"{{.Model}}", qc.Model,
		"{{.Variant}}", qc.Variant,
		"{{.Window}}", qc.Window,
		// Tolerated spellings. Someone will write them, and failing on a space
		// inside a brace would be a poor use of everybody's afternoon.
		"{{ .Model }}", qc.Model,
		"{{ .Variant }}", qc.Variant,
		"{{ .Window }}", qc.Window,
	).Replace(query)
}
