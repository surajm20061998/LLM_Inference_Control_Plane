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

package observability

import (
	"fmt"
	"maps"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// Recording rule names.
//
// The COLON is deliberate and is the one place in this project where it belongs.
// Prometheus reserves `:` in a metric name for recording rules by convention,
// which is precisely why the ServiceMonitor rewrites llama.cpp's `llamacpp:*`
// exported metrics to `llamacpp_*` — those are scraped series masquerading as
// recording rules. These genuinely are recording rules, so they get the colon.
//
// The `level:metric:operation` convention reads as "over what, of what, how":
// llmcp is the level, ttft_sli the metric, ratio_rate5m the operation.
const (
	recordTTFTSLI  = "llmcp:ttft_sli:ratio_rate"
	recordAvailSLI = "llmcp:availability_sli:ratio_rate"
)

// burnWindow is one rung of the multiwindow burn-rate ladder.
//
// # Why two windows per rung
//
// A single long window is slow to fire; a single short window is noisy. The
// Google SRE workbook's answer is to require BOTH: the long window establishes
// that the budget is genuinely being consumed at that rate, and the short window
// establishes that it still is right now. The short window is what stops an
// alert from staying lit for an hour after the incident is over.
type burnWindow struct {
	// Name is the alert suffix.
	Name string

	// Factor is how many times faster than the budget allows.
	//
	// 14.4 exhausts a 30-day budget in about two days and is the page. 6
	// exhausts it in five days and is a ticket. Both numbers come straight from
	// the SRE workbook's worked example and are chosen so that the alert fires
	// after roughly 2% and 5% of the budget is spent respectively.
	Factor float64

	// Long and Short are the two evaluation windows.
	Long  string
	Short string

	// Severity is the routing label.
	Severity string

	// For is how long the condition must hold before firing.
	For string
}

// fastBurnLadder is the tier that always ships.
//
// Both rungs use windows short enough to have data in a cluster that is minutes
// old, which is what makes them demonstrable rather than aspirational.
var fastBurnLadder = []burnWindow{
	{Name: "FastBurn", Factor: 14.4, Long: "1h", Short: "5m", Severity: severityCritical, For: "2m"},
	{Name: "SlowBurn", Factor: 6, Long: "6h", Short: "30m", Severity: severityWarning, For: "15m"},
}

// longBurnLadder is the tier gated behind spec.observability.prometheusRule.longWindowAlerts.
//
// These catch a budget being consumed steadily rather than suddenly, which is
// the failure mode the fast rungs miss entirely. They are off by default because
// a demo cluster has no 3d history, so every panel and alert built on them shows
// "No Data" — which looks worse than not having them and teaches a viewer to
// ignore the alert list.
var longBurnLadder = []burnWindow{
	{Name: "SlowBurnDaily", Factor: 3, Long: "1d", Short: "2h", Severity: severityWarning, For: "1h"},
	{Name: "SlowBurnWeekly", Factor: 1, Long: "3d", Short: "6h", Severity: severityInfo, For: "3h"},
}

// recordingWindows are the lookbacks the SLI recording rules materialise.
//
// Every window any enabled alert references must appear here, because the
// alerts read the recorded series rather than recomputing the ratio. That
// indirection is what keeps an alert expression readable, and — more
// importantly — it means the SLI is computed ONCE per window per evaluation
// instead of four times inside four alert expressions.
var (
	fastRecordingWindows = []string{"5m", "30m", "1h", "6h"}
	longRecordingWindows = []string{"2h", "6h", "1d", "3d"}
)

// PrometheusRuleInput is everything needed to render the rules.
type PrometheusRuleInput struct {
	Name      string
	Namespace string
	Model     string
	OwnerRef  metav1.OwnerReference
	Spec      *inferencev1alpha1.ObservabilitySpec
}

// SnappedThreshold reports the latency threshold the rules will actually use.
//
// Returned separately from the rendered object so the controller can tell the
// user their 0.5s objective became 0.6s. Silently changing a number someone
// wrote in a spec is the kind of helpfulness that costs an afternoon later.
func SnappedThreshold(spec *inferencev1alpha1.ObservabilitySpec) (value float64, changed bool) {
	var slo *inferencev1alpha1.SLOSpec
	if spec != nil {
		slo = spec.SLO
	}
	return llmcpmetrics.SnapToBucket(slo.ResolvedTTFTThreshold(), llmcpmetrics.TTFTBuckets)
}

// BuildPrometheusRule renders the recording rules and burn-rate alerts.
//
// Determinism matters as much here as it does for the ServiceMonitor: the
// result is applied with Server-Side Apply and a byte-unstable render would
// rewrite the object on every reconcile, fire the watch, and loop. Every slice
// below is built in a fixed order and the only map-derived value goes into a
// map, where encoding/json sorts it.
func BuildPrometheusRule(in PrometheusRuleInput) *unstructured.Unstructured {
	labels := naming.CommonLabels(in.Name)
	labels[naming.LabelComponent] = ComponentSLO
	if in.Spec != nil && in.Spec.PrometheusRule != nil {
		maps.Copy(labels, in.Spec.PrometheusRule.Labels)
	}

	var slo *inferencev1alpha1.SLOSpec
	if in.Spec != nil {
		slo = in.Spec.SLO
	}

	threshold, _ := SnappedThreshold(in.Spec)
	ttftObjective := slo.ResolvedTTFTObjective()
	availObjective := slo.ResolvedAvailabilityObjective()

	long := in.Spec.LongWindowAlertsEnabled()

	windows := append([]string{}, fastRecordingWindows...)
	ladder := append([]burnWindow{}, fastBurnLadder...)
	if long {
		windows = append(windows, longRecordingWindows...)
		ladder = append(ladder, longBurnLadder...)
	}
	windows = dedupe(windows)

	groups := []any{
		map[string]any{
			fieldName: in.Name + ".sli",
			// The recording interval is looser than the scrape interval on
			// purpose: an SLI over a 5m window does not become more accurate by
			// being recomputed every 15s, and each evaluation is a range query
			// over every series the selector matches.
			fieldInterval: "30s",
			fieldRules:    recordingRules(in.Model, threshold, windows),
		},
		map[string]any{
			fieldName:     in.Name + ".slo",
			fieldInterval: "30s",
			fieldRules:    alertRules(in, ttftObjective, availObjective, threshold, ladder),
		},
	}

	return &unstructured.Unstructured{Object: map[string]any{
		fieldAPIVersion: MonitoringGroup + "/" + MonitoringVersion,
		fieldKind:       KindPrometheusRule,
		fieldMetadata: map[string]any{
			fieldName:            naming.PrometheusRule(in.Name),
			fieldNamespace:       in.Namespace,
			fieldLabels:          toStringMap(labels),
			fieldOwnerReferences: []any{ownerRefMap(in.OwnerRef)},
		},
		fieldSpec: map[string]any{fieldGroups: groups},
	}}
}

// ComponentSLO is the app.kubernetes.io/component value on the generated rules.
const ComponentSLO = "slo"

// recordingRules materialises the two SLIs over every needed window.
//
// # The shape of a latency SLI
//
//	1 - (sum(rate(ttft_count[w])) - sum(rate(ttft_bucket{le="T"}[w])))
//	      / sum(rate(ttft_count[w]))
//
// which reads "the fraction of requests faster than T". Note the denominator is
// the histogram's own _count, not a separate request counter: they can differ,
// because TTFT is recorded only for STREAMED responses, and dividing streamed
// successes by all requests would report an SLI that falls whenever a client
// sends a non-streaming request. Using the histogram's own count keeps the
// numerator and denominator over the same population.
//
// # Why BAD/total, subtracted from one, rather than the obvious good/total
//
// Both SLIs are written as `1 - bad/total` so that ZERO TRAFFIC yields a
// perfect 1.0. The direct form does not, and the difference pages people.
//
// A plain (non-Vec) histogram is exported from the very first scrape with
// _count at 0 and every bucket present, so on an idle deployment the good/total
// form evaluates to 0 / 1e-9 = 0 — a zero SLI, not a perfect one. The burn-rate
// ladder below then reads (1 - 0) as a 100% error rate and fires
// LLMCPTTFTBudgetFastBurn, critical, about two minutes after start-up, on a
// deployment that has simply not been asked for anything yet. LLMCPNoTraffic
// exists precisely to cover that state, and it cannot do its job if the burn
// alerts fire there first.
//
// The bad/total form is 0/1e-9 = 0 in the same state, so the SLI is 1.0 and
// only LLMCPNoTraffic speaks.
//
// The availability numerator needs one more guard. A counter series that was
// never incremented does not exist, and the shim pre-initialises only
// code="200" — so `{code=~"5.."}` selects NOTHING while the deployment is
// healthy, sum() over no series is an EMPTY vector rather than zero, and every
// operator involving it yields empty. Without `or vector(0)` the recording rule
// produces no samples at all in the healthy case, and the SLO dashboard renders
// "No Data" for a service that is working perfectly.
//
// The `le` value must be an exact bucket boundary and is formatted the way the
// client library formats one. See internal/metrics/buckets.go.
func recordingRules(model string, threshold float64, windows []string) []any {
	le := llmcpmetrics.FormatBucket(threshold)
	sel := fmt.Sprintf(`%s=%q`, llmcpmetrics.LabelModel, model)

	rules := make([]any, 0, len(windows)*2)

	for _, w := range windows {
		rules = append(rules, map[string]any{
			fieldRecord: recordTTFTSLI + w,
			fieldExpr: fmt.Sprintf(
				`1 - (clamp_min(sum(rate(%s_count{%s}[%s])) - sum(rate(%s_bucket{%s,le=%q}[%s])), 0) `+
					`/ clamp_min(sum(rate(%s_count{%s}[%s])), 1e-9))`,
				llmcpmetrics.TTFTSeconds, sel, w,
				llmcpmetrics.TTFTSeconds, sel, le, w,
				llmcpmetrics.TTFTSeconds, sel, w),
			fieldLabels: map[string]any{llmcpmetrics.LabelModel: model},
		})
	}

	for _, w := range windows {
		rules = append(rules, map[string]any{
			fieldRecord: recordAvailSLI + w,
			fieldExpr: fmt.Sprintf(
				`1 - ((sum(rate(%s{%s,%s=~"5.."}[%s])) or vector(0)) `+
					`/ clamp_min(sum(rate(%s{%s}[%s])), 1e-9))`,
				llmcpmetrics.RequestsTotal, sel, llmcpmetrics.LabelCode, w,
				llmcpmetrics.RequestsTotal, sel, w),
			fieldLabels: map[string]any{llmcpmetrics.LabelModel: model},
		})
	}

	return rules
}

// alertRules renders the burn-rate ladder for both SLOs.
func alertRules(
	in PrometheusRuleInput,
	ttftObjective, availObjective, threshold float64,
	ladder []burnWindow,
) []any {
	rules := make([]any, 0, len(ladder)*2+1)

	for _, rung := range ladder {
		rules = append(rules, burnAlert(burnAlertInput{
			Alert:     "LLMCPTTFTBudget" + rung.Name,
			Record:    recordTTFTSLI,
			Objective: ttftObjective,
			Rung:      rung,
			MD:        in,
			Summary: fmt.Sprintf(
				"Time-to-first-token budget for %s is burning at %.4gx", in.Name, rung.Factor),
			Description: fmt.Sprintf(
				"Over the last %s and the last %s, more than %.4gx the allowed share of requests "+
					"took longer than %gs to produce their first token. The %.4g%% objective's error "+
					"budget is being consumed %.4gx faster than sustainable.",
				rung.Long, rung.Short, rung.Factor, threshold, ttftObjective*100, rung.Factor),
			SLO: "ttft",
		}))
	}

	for _, rung := range ladder {
		rules = append(rules, burnAlert(burnAlertInput{
			Alert:     "LLMCPAvailabilityBudget" + rung.Name,
			Record:    recordAvailSLI,
			Objective: availObjective,
			Rung:      rung,
			MD:        in,
			Summary: fmt.Sprintf(
				"Availability budget for %s is burning at %.4gx", in.Name, rung.Factor),
			Description: fmt.Sprintf(
				"Over the last %s and the last %s, more than %.4gx the allowed share of requests "+
					"failed with a 5xx. The %.4g%% objective's error budget is being consumed "+
					"%.4gx faster than sustainable.",
				rung.Long, rung.Short, rung.Factor, availObjective*100, rung.Factor),
			SLO: "availability",
		}))
	}

	rules = append(rules, noTrafficAlert(in))

	return rules
}

// burnAlertInput is one alert's parameters.
type burnAlertInput struct {
	Alert       string
	Record      string
	Objective   float64
	Rung        burnWindow
	MD          PrometheusRuleInput
	Summary     string
	Description string
	SLO         string
}

// burnAlert renders one rung of the ladder.
//
// The expression is the multiwindow form: BOTH the long and the short window
// must show the budget burning faster than the factor allows. The long window
// establishes that this is a real trend; the short window establishes that it
// is still happening, which is what stops the alert staying lit for an hour
// after the incident ended.
func burnAlert(in burnAlertInput) map[string]any {
	budget := 1 - in.Objective
	limit := in.Rung.Factor * budget

	expr := fmt.Sprintf(
		"(1 - %s%s{%s=%q}) > %s\nand\n(1 - %s%s{%s=%q}) > %s",
		in.Record, in.Rung.Long, llmcpmetrics.LabelModel, in.MD.Model, formatG(limit),
		in.Record, in.Rung.Short, llmcpmetrics.LabelModel, in.MD.Model, formatG(limit))

	return map[string]any{
		fieldAlert: in.Alert,
		fieldExpr:  expr,
		fieldFor:   in.Rung.For,
		fieldLabels: map[string]any{
			labelSeverity:               in.Rung.Severity,
			labelSLO:                    in.SLO,
			llmcpmetrics.LabelModel:     in.MD.Model,
			naming.LabelModelDeployment: in.MD.Name,
			// The namespace is a label rather than only metadata so that a
			// routing tree can send a team's alerts to that team without
			// needing to know which ModelDeployments they own.
			fieldNamespace: in.MD.Namespace,
		},
		fieldAnnotations: map[string]any{
			annotationSummary: in.Summary,
			annotationDesc:    in.Description,
			// A runbook annotation is conventional and Alertmanager templates
			// expect it. Pointing at the project's own docs is more useful than
			// omitting it, and far more useful than a placeholder URL.
			annotationRunbook: runbookURL,
		},
	}
}

// noTrafficAlert fires when a ModelDeployment stops receiving requests
// entirely.
//
// # Why this exists alongside the burn-rate alerts
//
// Because the burn-rate alerts cannot fire without traffic, and that is not a
// gap in them — it is arithmetic. With no requests, both SLIs are ratios with a
// clamped denominator, so they evaluate to a perfect 1.0 and every budget alert
// stays silent. A ModelDeployment whose Service selector broke, or whose shim
// stopped being scraped, therefore looks flawless on every SLO panel.
//
// The distinction this alert draws is the same one the canary analysis draws
// with minRequestRate: "no problems" and "no data" are different states, and a
// system that cannot tell them apart reports the second as the first.
func noTrafficAlert(in PrometheusRuleInput) map[string]any {
	return map[string]any{
		fieldAlert: alertNoTraffic,
		fieldExpr: fmt.Sprintf(
			`sum(rate(%s{%s=%q}[10m])) == 0 and on() (sum(%s{%s=%q}) > 0)`,
			llmcpmetrics.RequestsTotal, llmcpmetrics.LabelModel, in.Model,
			llmcpmetrics.ShimInfo, llmcpmetrics.LabelModel, in.Model),
		// Fifteen minutes, because a genuinely idle deployment is normal
		// overnight and this must not page for it. It is a warning, not a page.
		fieldFor: "15m",
		fieldLabels: map[string]any{
			labelSeverity:               severityWarning,
			labelSLO:                    "traffic",
			llmcpmetrics.LabelModel:     in.Model,
			naming.LabelModelDeployment: in.Name,
			fieldNamespace:              in.Namespace,
		},
		fieldAnnotations: map[string]any{
			annotationSummary: fmt.Sprintf("%s has shims reporting but no requests for 10 minutes", in.Name),
			annotationDesc: "The pods are up and being scraped, but nothing is reaching them. " +
				"Every SLO for this deployment currently evaluates to a perfect score because " +
				"both SLIs are ratios over zero traffic — so this alert, not those, is the one " +
				"that can tell you the difference between 'no problems' and 'no data'.",
			annotationRunbook: runbookURL,
		},
	}
}

// formatG renders a float for a PromQL literal without trailing noise.
func formatG(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// dedupe removes duplicate windows, preserving first-seen order.
//
// Order-preserving rather than sorted, because the result becomes a rule slice
// and Server-Side Apply needs it byte-stable. Sorting would also be stable, but
// preserving the declaration order keeps the generated object readable in the
// order the ladders are defined.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
