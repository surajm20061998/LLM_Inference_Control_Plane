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
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

// testOwnerUID is the fixture ModelDeployment's UID. A real value matters:
// an owner reference with an empty or stale UID makes garbage collection either
// delete the child immediately or never.
const testOwnerUID = "uid-1"

func ruleInput(mutate ...func(*PrometheusRuleInput)) PrometheusRuleInput {
	in := PrometheusRuleInput{
		Name:      testMDName,
		Namespace: testNamespace,
		Model:     "qwen3",
		OwnerRef: metav1.OwnerReference{
			APIVersion: "inference.llmcp.io/v1alpha1",
			Kind:       "ModelDeployment",
			Name:       testMDName,
			UID:        testOwnerUID,
			Controller: ptr.To(true),
		},
		Spec: &inferencev1alpha1.ObservabilitySpec{},
	}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// ruleGroups returns the generated groups.
func ruleGroups(t *testing.T, u *unstructured.Unstructured) []any {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(u.Object, fieldSpec, fieldGroups)
	if err != nil || !found {
		t.Fatalf("spec.groups missing: found=%v err=%v", found, err)
	}
	return groups
}

// allRules flattens every rule across every group.
func allRules(t *testing.T, u *unstructured.Unstructured) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, g := range ruleGroups(t, u) {
		group := g.(map[string]any)
		rules, _ := group[fieldRules].([]any)
		for _, r := range rules {
			out = append(out, r.(map[string]any))
		}
	}
	return out
}

// recordedNames returns every series the recording rules define.
func recordedNames(t *testing.T, u *unstructured.Unstructured) []string {
	t.Helper()
	var out []string
	for _, r := range allRules(t, u) {
		if name, ok := r[fieldRecord].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// alerts returns every alerting rule.
func alerts(t *testing.T, u *unstructured.Unstructured) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range allRules(t, u) {
		if _, ok := r[fieldAlert].(string); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestPrometheusRuleShape(t *testing.T) {
	t.Parallel()

	u := BuildPrometheusRule(ruleInput())

	if got := u.GetKind(); got != KindPrometheusRule {
		t.Errorf("kind = %q", got)
	}
	if got := u.GetName(); got != testMDName+"-slo" {
		t.Errorf("name = %q, want %q", got, testMDName+"-slo")
	}
	if got := u.GetNamespace(); got != testNamespace {
		t.Errorf("namespace = %q", got)
	}

	// Ownership is what removes the rules with the ModelDeployment, and it is
	// why this operator needs no finalizer.
	refs, found, _ := unstructured.NestedSlice(u.Object, fieldMetadata, fieldOwnerReferences)
	if !found || len(refs) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly one", refs)
	}
}

// TestEveryAlertReferencesADefinedRecordingRule is the same class of check as
// the dashboard one, applied to alerts.
//
// An alert whose expression names a recording rule that was never defined does
// not error: the query returns an empty vector, the comparison is false, and
// the alert simply never fires. An SLO that cannot alert is worse than no SLO,
// because it is believed.
func TestEveryAlertReferencesADefinedRecordingRule(t *testing.T) {
	t.Parallel()

	for _, long := range []bool{false, true} {
		u := BuildPrometheusRule(ruleInput(func(in *PrometheusRuleInput) {
			in.Spec = &inferencev1alpha1.ObservabilitySpec{
				PrometheusRule: &inferencev1alpha1.PrometheusRuleSpec{
					LongWindowAlerts: ptr.To(long),
				},
			}
		}))

		defined := map[string]bool{}
		for _, name := range recordedNames(t, u) {
			defined[name] = true
		}

		for _, a := range alerts(t, u) {
			expr := a[fieldExpr].(string)
			for _, ident := range seriesNamesIn(expr) {
				if !strings.HasPrefix(ident, "llmcp:") {
					continue
				}
				if !defined[ident] {
					t.Errorf("longWindowAlerts=%v: alert %s references recording rule %q, "+
						"which is not defined — the alert would never fire",
						long, a[fieldAlert], ident)
				}
			}
		}
	}
}

func TestLatencySLIUsesAnExactBucketBoundary(t *testing.T) {
	t.Parallel()

	// Prometheus matches `le` as an exact STRING. A rule asking for a boundary
	// the histogram does not have returns an empty vector rather than an error,
	// so the recording rule produces nothing and the alert never fires.
	u := BuildPrometheusRule(ruleInput(func(in *PrometheusRuleInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			SLO: &inferencev1alpha1.SLOSpec{TTFTThreshold: quantity("1500m")},
		}
	}))

	var found bool
	for _, r := range allRules(t, u) {
		name, _ := r[fieldRecord].(string)
		if !strings.HasPrefix(name, recordTTFTSLI) {
			continue
		}
		expr := r[fieldExpr].(string)
		found = true

		le := extractLe(t, expr)
		if le == "" {
			t.Fatalf("no le selector in %s", expr)
		}

		var isBucket bool
		for _, b := range llmcpmetrics.TTFTBuckets {
			if llmcpmetrics.FormatBucket(b) == le {
				isBucket = true
				break
			}
		}
		if !isBucket {
			t.Errorf("le=%q is not one of the histogram's bucket boundaries; the rule would "+
				"select nothing. Boundaries are %v", le, llmcpmetrics.TTFTBuckets)
		}
	}
	if !found {
		t.Fatal("no TTFT SLI recording rule was generated")
	}
}

// extractLe pulls the le selector value out of an expression.
func extractLe(t *testing.T, expr string) string {
	t.Helper()
	_, rest, found := strings.Cut(expr, `le="`)
	if !found {
		return ""
	}
	value, _, found := strings.Cut(rest, `"`)
	if !found {
		return ""
	}
	return value
}

func TestThresholdSnapsUpToTheNextBoundary(t *testing.T) {
	t.Parallel()

	// Rounding UP makes the objective slightly easier than written. Rounding
	// down would silently hold a service to a stricter target than its owner
	// declared, which is how an SLO burns budget for a latency nobody
	// considered a violation.
	cases := []struct {
		threshold string
		want      float64
		changed   bool
	}{
		{threshold: "1500m", want: 1.5, changed: false}, // already a boundary
		{threshold: "500m", want: 0.6, changed: true},   // between 0.4 and 0.6
		{threshold: "1", want: 1, changed: false},
		{threshold: "12", want: 15, changed: true},
		{threshold: "1m", want: 0.01, changed: true}, // below every boundary
		{threshold: "500", want: 30, changed: true},  // above every boundary
	}

	for _, tc := range cases {
		spec := &inferencev1alpha1.ObservabilitySpec{
			SLO: &inferencev1alpha1.SLOSpec{TTFTThreshold: quantity(tc.threshold)},
		}
		got, changed := SnappedThreshold(spec)
		if got != tc.want || changed != tc.changed {
			t.Errorf("threshold %s -> (%v, %v), want (%v, %v)",
				tc.threshold, got, changed, tc.want, tc.changed)
		}
	}
}

func TestThresholdNeverSnapsToInfinity(t *testing.T) {
	t.Parallel()

	// `le="+Inf"` is a valid selector but it selects EVERY request, so the
	// ratio would be a constant 1.0 and the SLI would report perfect health
	// forever.
	spec := &inferencev1alpha1.ObservabilitySpec{
		SLO: &inferencev1alpha1.SLOSpec{TTFTThreshold: quantity("100000")},
	}
	got, _ := SnappedThreshold(spec)

	last := llmcpmetrics.TTFTBuckets[len(llmcpmetrics.TTFTBuckets)-1]
	if got != last {
		t.Fatalf("snapped to %v, want the last finite boundary %v", got, last)
	}
}

func TestFastBurnLadderAlwaysShips(t *testing.T) {
	t.Parallel()

	u := BuildPrometheusRule(ruleInput())

	names := map[string]bool{}
	for _, a := range alerts(t, u) {
		names[a[fieldAlert].(string)] = true
	}

	// Both SLOs get both fast rungs, plus the no-traffic alert.
	for _, want := range []string{
		"LLMCPTTFTBudgetFastBurn",
		"LLMCPTTFTBudgetSlowBurn",
		"LLMCPAvailabilityBudgetFastBurn",
		"LLMCPAvailabilityBudgetSlowBurn",
		"LLMCPNoTraffic",
	} {
		if !names[want] {
			t.Errorf("alert %s is missing", want)
		}
	}

	// The long-window tier is OFF by default: a demo cluster is minutes old, so
	// a 3d window has no data and every alert built on it shows "No Data" —
	// which looks worse than not having them.
	for _, absent := range []string{"LLMCPTTFTBudgetSlowBurnDaily", "LLMCPTTFTBudgetSlowBurnWeekly"} {
		if names[absent] {
			t.Errorf("alert %s should be gated behind longWindowAlerts", absent)
		}
	}
}

func TestLongWindowAlertsCanBeEnabled(t *testing.T) {
	t.Parallel()

	u := BuildPrometheusRule(ruleInput(func(in *PrometheusRuleInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			PrometheusRule: &inferencev1alpha1.PrometheusRuleSpec{
				LongWindowAlerts: ptr.To(true),
			},
		}
	}))

	names := map[string]bool{}
	for _, a := range alerts(t, u) {
		names[a[fieldAlert].(string)] = true
	}
	for _, want := range []string{"LLMCPTTFTBudgetSlowBurnDaily", "LLMCPTTFTBudgetSlowBurnWeekly"} {
		if !names[want] {
			t.Errorf("alert %s is missing although longWindowAlerts is true", want)
		}
	}
}

func TestBurnRateThresholdsScaleWithTheObjective(t *testing.T) {
	t.Parallel()

	// A stricter objective makes every alert MORE sensitive rather than merely
	// raising a bar: the budget is 1 - objective, and the alert fires at
	// factor x budget.
	loose := BuildPrometheusRule(ruleInput(func(in *PrometheusRuleInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			SLO: &inferencev1alpha1.SLOSpec{TTFTObjective: quantity("900m")}, // 90%, 10% budget
		}
	}))
	strict := BuildPrometheusRule(ruleInput(func(in *PrometheusRuleInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			SLO: &inferencev1alpha1.SLOSpec{TTFTObjective: quantity("999m")}, // 99.9%, 0.1% budget
		}
	}))

	looseLimit := fastBurnLimit(t, loose, "LLMCPTTFTBudgetFastBurn")
	strictLimit := fastBurnLimit(t, strict, "LLMCPTTFTBudgetFastBurn")

	if strictLimit >= looseLimit {
		t.Fatalf("a stricter objective produced a looser alert threshold: strict=%s loose=%s",
			strictLimit, looseLimit)
	}
}

// fastBurnLimit extracts the numeric threshold from an alert's expression.
func fastBurnLimit(t *testing.T, u *unstructured.Unstructured, alert string) string {
	t.Helper()
	for _, a := range alerts(t, u) {
		if a[fieldAlert] != alert {
			continue
		}
		expr := a[fieldExpr].(string)
		_, rest, found := strings.Cut(expr, "> ")
		if !found {
			t.Fatalf("no comparison in %s", expr)
		}
		line, _, _ := strings.Cut(rest, "\n")
		return strings.TrimSpace(line)
	}
	t.Fatalf("alert %s not found", alert)
	return ""
}

func TestBurnAlertsRequireBothWindows(t *testing.T) {
	t.Parallel()

	// The multiwindow form. The long window establishes the budget is genuinely
	// being consumed at that rate; the short one establishes it still is, which
	// is what stops an alert staying lit for an hour after the incident ended.
	u := BuildPrometheusRule(ruleInput())

	for _, a := range alerts(t, u) {
		name := a[fieldAlert].(string)
		if !strings.Contains(name, "Budget") {
			continue
		}
		expr := a[fieldExpr].(string)
		if !strings.Contains(expr, "\nand\n") {
			t.Errorf("alert %s is not a multiwindow expression: %s", name, expr)
		}
	}
}

func TestNoTrafficAlertExists(t *testing.T) {
	t.Parallel()

	// The burn-rate alerts CANNOT fire without traffic, and that is arithmetic
	// rather than a gap: with no requests both SLIs are ratios over a clamped
	// denominator, so they evaluate to a perfect 1.0 and stay silent. A
	// deployment whose Service selector broke would look flawless.
	u := BuildPrometheusRule(ruleInput())

	for _, a := range alerts(t, u) {
		if a[fieldAlert] != alertNoTraffic {
			continue
		}
		expr := a[fieldExpr].(string)
		if !strings.Contains(expr, llmcpmetrics.ShimInfo) {
			t.Errorf("the no-traffic alert must confirm the shims are REPORTING, or it would "+
				"fire for a deployment that was simply scaled to zero: %s", expr)
		}
		return
	}
	t.Fatal(alertNoTraffic + " is missing")
}

func TestEveryRatioClampsItsDenominatorInRules(t *testing.T) {
	t.Parallel()

	// Same trap as the analysis queries: division by zero yields NaN, NaN
	// compares false against every threshold, and the SLI silently reports
	// perfect health.
	u := BuildPrometheusRule(ruleInput())

	for _, r := range allRules(t, u) {
		name, ok := r[fieldRecord].(string)
		if !ok {
			continue
		}
		expr := r[fieldExpr].(string)
		if strings.Contains(expr, "/") && !strings.Contains(expr, "clamp_min") {
			t.Errorf("recording rule %s divides without clamping its denominator: %s", name, expr)
		}
	}
}

func TestLatencySLIDividesByTheHistogramsOwnCount(t *testing.T) {
	t.Parallel()

	// Not by a separate request counter. TTFT is recorded only for STREAMED
	// responses, so dividing streamed successes by ALL requests would report an
	// SLI that falls whenever a client sends a non-streaming request.
	u := BuildPrometheusRule(ruleInput())

	for _, r := range allRules(t, u) {
		name, _ := r[fieldRecord].(string)
		if !strings.HasPrefix(name, recordTTFTSLI) {
			continue
		}
		expr := r[fieldExpr].(string)
		if !strings.Contains(expr, llmcpmetrics.TTFTSeconds+"_count") {
			t.Errorf("%s does not divide by the histogram's own _count: %s", name, expr)
		}
		if strings.Contains(expr, llmcpmetrics.RequestsTotal) {
			t.Errorf("%s divides by the request counter, whose population differs from the "+
				"histogram's: %s", name, expr)
		}
	}
}

func TestBuildPrometheusRuleIsDeterministic(t *testing.T) {
	t.Parallel()

	// Applied with Server-Side Apply. A byte-unstable render rewrites the object
	// on every reconcile, fires the watch, and loops — invisible until it is
	// saturating the API server. Go randomises map iteration per range
	// statement, so a single comparison would pass by luck.
	in := ruleInput(func(in *PrometheusRuleInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			PrometheusRule: &inferencev1alpha1.PrometheusRuleSpec{
				LongWindowAlerts: ptr.To(true),
				Labels:           map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"},
			},
			SLO: &inferencev1alpha1.SLOSpec{TTFTThreshold: quantity("600m")},
		}
	})

	first, err := json.Marshal(BuildPrometheusRule(in).Object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 200 {
		got, err := json.Marshal(BuildPrometheusRule(in).Object)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d produced different bytes; SSA would loop forever", i)
		}
	}
}

func TestRecordingRulesUseTheReservedColon(t *testing.T) {
	t.Parallel()

	// The one place in this project where a colon in a metric name belongs.
	// Prometheus reserves it for recording rules by convention — which is
	// exactly why the ServiceMonitor rewrites llama.cpp's exported `llamacpp:*`
	// series to underscores. Those are scraped metrics masquerading as
	// recording rules; these genuinely are recording rules.
	u := BuildPrometheusRule(ruleInput())

	names := recordedNames(t, u)
	if len(names) == 0 {
		t.Fatal("no recording rules were generated")
	}
	for _, n := range names {
		if !strings.Contains(n, ":") {
			t.Errorf("recording rule %q has no colon; the convention is level:metric:operation", n)
		}
	}
}

func TestAlertsCarryRoutingLabels(t *testing.T) {
	t.Parallel()

	// A routing tree has to be able to send a team's alerts to that team
	// without knowing which ModelDeployments they own, which means the
	// namespace has to be a LABEL and not only metadata.
	u := BuildPrometheusRule(ruleInput())

	for _, a := range alerts(t, u) {
		labels, _ := a[fieldLabels].(map[string]any)
		for _, key := range []string{labelSeverity, fieldNamespace, llmcpmetrics.LabelModel} {
			if labels[key] == nil || labels[key] == "" {
				t.Errorf("alert %s has no %s label", a[fieldAlert], key)
			}
		}
		annotations, _ := a[fieldAnnotations].(map[string]any)
		for _, key := range []string{annotationSummary, annotationDesc, annotationRunbook} {
			if annotations[key] == nil || annotations[key] == "" {
				t.Errorf("alert %s has no %s annotation", a[fieldAlert], key)
			}
		}
	}
}

func TestObjectiveOutsideTheOpenIntervalFallsBack(t *testing.T) {
	t.Parallel()

	// An objective of exactly 1.0 makes the error budget zero and every
	// burn-rate expression a division by zero — which yields +Inf and fires
	// every alert permanently.
	for _, bad := range []string{"1", "0", "2", "-500m"} {
		slo := &inferencev1alpha1.SLOSpec{TTFTObjective: quantity(bad)}
		got := slo.ResolvedTTFTObjective()
		if got <= 0 || got >= 1 {
			t.Errorf("objective %q resolved to %v, which is outside (0,1)", bad, got)
		}
	}
}
