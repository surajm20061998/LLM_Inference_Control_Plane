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
	"strings"
	"testing"
	"time"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Fixture values shared across this package's tests. They are constants so that
// a golden query string and the context it was rendered from cannot drift.
const (
	testNamespace       = "team-a"
	testModelDeployment = "chat"
	testModel           = "qwen3"
	testWindow          = "60s"
	// trivialQuery renders to something valid without depending on any metric
	// name, for cases whose subject is the evaluation rather than the query.
	trivialQuery = "vector(1)"
)

var canaryCtx = QueryContext{
	Namespace: testNamespace, ModelDeployment: testModelDeployment,
	Model: testModel, Variant: "canary", Window: testWindow,
}

func builtin(b inferencev1alpha1.BuiltinMetric) inferencev1alpha1.AnalysisMetric {
	return inferencev1alpha1.AnalysisMetric{Name: string(b), Builtin: &b}
}

// TestBuiltinQueriesAreGolden pins the exact PromQL each built-in produces.
//
// Golden strings rather than "contains" assertions, because the failure mode
// being guarded against is subtle: a query that is nearly right returns a
// number, and a rollout is gated on it. An aggregation over the wrong labels,
// a missing rate(), a selector that omits the variant — none of them error, all
// of them silently change what is being measured.
func TestBuiltinQueriesAreGolden(t *testing.T) {
	t.Parallel()

	cases := map[inferencev1alpha1.BuiltinMetric]string{
		inferencev1alpha1.MetricTTFTP95: `histogram_quantile(0.95, sum by (le) ` +
			`(rate(llmcp_inference_ttft_seconds_bucket{namespace="team-a",model_deployment="chat",variant="canary"}[60s])))`,

		inferencev1alpha1.MetricTTFTP99: `histogram_quantile(0.99, sum by (le) ` +
			`(rate(llmcp_inference_ttft_seconds_bucket{namespace="team-a",model_deployment="chat",variant="canary"}[60s])))`,

		inferencev1alpha1.MetricRequestDurationP95: `histogram_quantile(0.95, sum by (le) ` +
			`(rate(llmcp_inference_request_duration_seconds_bucket{namespace="team-a",model_deployment="chat",variant="canary"}[60s])))`,

		inferencev1alpha1.MetricErrorRate: `sum(rate(llmcp_inference_requests_total` +
			`{namespace="team-a",model_deployment="chat",variant="canary",code=~"5.."}[60s])) / ` +
			`clamp_min(sum(rate(llmcp_inference_requests_total{namespace="team-a",model_deployment="chat",variant="canary"}[60s])), 1e-9)`,

		inferencev1alpha1.MetricQueueDepth: `avg(avg_over_time(llmcp_inference_queue_depth` +
			`{namespace="team-a",model_deployment="chat",variant="canary"}[60s]))`,

		inferencev1alpha1.MetricOutputTokenRate: `sum(rate(llmcp_inference_reported_output_tokens_total` +
			`{namespace="team-a",model_deployment="chat",variant="canary"}[60s]))`,
		inferencev1alpha1.MetricOutputChunkRate: `sum(rate(llmcp_inference_output_chunks_total` +
			`{namespace="team-a",model_deployment="chat",variant="canary"}[60s]))`,
	}

	for b, want := range cases {
		got, err := Query(builtin(b), canaryCtx)
		if err != nil {
			t.Errorf("%s: %v", b, err)
			continue
		}
		if got != want {
			t.Errorf("%s:\n got: %s\nwant: %s", b, got, want)
		}
	}
}

func TestEveryRatioClampsItsDenominator(t *testing.T) {
	t.Parallel()

	// PromQL division by zero yields NaN when the numerator is also zero, and
	// NaN compares FALSE against every threshold — so an unclamped ratio
	// reports PASS for a canary that served nothing. Every query containing a
	// "/" must therefore contain a clamp_min.
	for _, b := range []inferencev1alpha1.BuiltinMetric{
		inferencev1alpha1.MetricErrorRate,
		inferencev1alpha1.MetricSuccessRate,
	} {
		got, err := Query(builtin(b), canaryCtx)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if strings.Contains(got, "/") && !strings.Contains(got, "clamp_min") {
			t.Errorf("%s divides without clamping its denominator: %s", b, got)
		}
	}
}

func TestSuccessRateIsOneMinusErrorRate(t *testing.T) {
	t.Parallel()

	// NOT a ratio of 2xx over total. Counting successes as "status 2xx" would
	// classify a 4xx — a client's malformed prompt — as a server failure, and a
	// canary would be rolled back because somebody sent it bad JSON.
	errRate, _ := Query(builtin(inferencev1alpha1.MetricErrorRate), canaryCtx)
	success, _ := Query(builtin(inferencev1alpha1.MetricSuccessRate), canaryCtx)

	if success != "1 - ("+errRate+")" {
		t.Errorf("success-rate = %s; it must be derived from the 5xx error rate", success)
	}
	if strings.Contains(success, `code=~"2..`) {
		t.Error("success-rate counts 2xx directly, which treats a client's 4xx as a server failure")
	}
}

func TestEverySelectorPinsBothLabels(t *testing.T) {
	t.Parallel()

	// Omitting `variant` would merge the canary's series with the primary's,
	// which for a comparison between the two is an inversion rather than an
	// imprecision: the regression would be diluted by exactly the traffic it
	// was supposed to be detected in.
	for _, b := range []inferencev1alpha1.BuiltinMetric{
		inferencev1alpha1.MetricTTFTP95,
		inferencev1alpha1.MetricTTFTP99,
		inferencev1alpha1.MetricRequestDurationP95,
		inferencev1alpha1.MetricErrorRate,
		inferencev1alpha1.MetricSuccessRate,
		inferencev1alpha1.MetricQueueDepth,
		inferencev1alpha1.MetricOutputTokenRate,
	} {
		got, err := Query(builtin(b), canaryCtx)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if !strings.Contains(got, `namespace="team-a",model_deployment="chat"`) || strings.Contains(got, `model=`) {
			t.Errorf("%s must pin resource identity, not the changing model label: %s", b, got)
		}
		if !strings.Contains(got, `variant="canary"`) {
			t.Errorf("%s does not pin the variant label: %s", b, got)
		}
	}
}

func TestPercentilesAggregateByLe(t *testing.T) {
	t.Parallel()

	// Without `sum by (le)` the expression yields one series per scraped pod,
	// which the provider reports as more than one sample and Evaluate turns
	// into an Error — the good outcome. The bad outcome is aggregating over the
	// WRONG labels, which averages a canary's pods with its primary's and
	// produces a percentile that always sits between the two.
	for _, b := range []inferencev1alpha1.BuiltinMetric{
		inferencev1alpha1.MetricTTFTP95,
		inferencev1alpha1.MetricRequestDurationP95,
	} {
		got, _ := Query(builtin(b), canaryCtx)
		if !strings.Contains(got, "sum by (le)") {
			t.Errorf("%s does not aggregate by le: %s", b, got)
		}
	}
}

func TestRawQuerySubstitution(t *testing.T) {
	t.Parallel()

	m := inferencev1alpha1.AnalysisMetric{
		Name:  "custom",
		Query: `sum(rate(my_metric{namespace="{{.Namespace}}",model_deployment="{{ .ModelDeployment }}",model="{{.Model}}",variant="{{ .Variant }}"}[{{.Window}}]))`,
	}
	got, err := Query(m, canaryCtx)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := `sum(rate(my_metric{namespace="team-a",model_deployment="chat",model="qwen3",variant="canary"}[60s]))`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRawQueryWinsOverBuiltin(t *testing.T) {
	t.Parallel()

	// CEL rejects setting both, but the renderer must still be unambiguous for
	// an object written before that validation existed.
	b := inferencev1alpha1.MetricTTFTP95
	m := inferencev1alpha1.AnalysisMetric{Name: "x", Builtin: &b, Query: trivialQuery}

	got, err := Query(m, canaryCtx)
	if err != nil || got != trivialQuery {
		t.Fatalf("Query = (%q, %v), want the raw query", got, err)
	}
}

func TestQueryRejectsAnEmptyMetric(t *testing.T) {
	t.Parallel()

	if _, err := Query(inferencev1alpha1.AnalysisMetric{Name: "x"}, canaryCtx); err == nil {
		t.Fatal("a metric with neither builtin nor query must be rejected")
	}
	unknown := inferencev1alpha1.BuiltinMetric("no-such-metric")
	if _, err := Query(inferencev1alpha1.AnalysisMetric{Name: "x", Builtin: &unknown}, canaryCtx); err == nil {
		t.Fatal("an unknown builtin must be rejected rather than rendering an empty query")
	}
}

func TestRatioQueryClampsThePrimary(t *testing.T) {
	t.Parallel()

	// A primary that reported nothing must not make the canary look infinitely
	// bad.
	got := RatioQuery("canary_expr", "primary_expr")
	if got != "(canary_expr) / clamp_min(primary_expr, 1e-9)" {
		t.Errorf("RatioQuery = %s", got)
	}
}

func TestRequestRateQuery(t *testing.T) {
	t.Parallel()

	want := `sum(rate(llmcp_inference_requests_total{namespace="team-a",model_deployment="chat",variant="canary"}[60s]))`
	if got, err := RequestRateQuery(canaryCtx); err != nil || got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestQueriesRejectMissingResourceIdentity(t *testing.T) {
	for _, qc := range []QueryContext{
		{Model: testModel, Window: testWindow},
		{Namespace: testNamespace, Model: testModel, Window: testWindow},
		{ModelDeployment: testModelDeployment, Model: testModel, Window: testWindow},
	} {
		if _, err := Query(builtin(inferencev1alpha1.MetricErrorRate), qc); err == nil {
			t.Error("built-in accepted missing identity, which would match legacy series")
		}
		if _, err := AutoscalingQuery(inferencev1alpha1.AutoscalingQueueDepth, qc); err == nil {
			t.Error("autoscaling accepted missing identity")
		}
		if _, err := RequestRateQuery(qc); err == nil {
			t.Error("traffic gate accepted missing identity")
		}
	}
}

func TestQueryEscapesLabelValues(t *testing.T) {
	qc := QueryContext{
		Namespace: testNamespace, ModelDeployment: testModelDeployment,
		Model: "a\"\\\nb", Variant: "canary", Window: testWindow,
	}
	want := `namespace="team-a",model_deployment="chat",variant="canary"`
	if got := selector(qc); got != want {
		t.Fatalf("selector = %q, want %q", got, want)
	}
	query := `metric{namespace="{{ .Namespace }}",model_deployment="{{.ModelDeployment}}",model="{{ .Model }}",variant="{{.Variant}}"}`
	if got := substitute(query, qc); got != `metric{namespace="team-a",model_deployment="chat",model="a\"\\\nb",variant="canary"}` {
		t.Fatalf("custom query = %q", got)
	}
}

func TestModelChangePreservesResourceQueryScope(t *testing.T) {
	stable, candidate := canaryCtx, canaryCtx
	stable.Model, candidate.Model = "old-model", "new-model"
	for _, render := range []func(QueryContext) (string, error){
		func(qc QueryContext) (string, error) { return Query(builtin(inferencev1alpha1.MetricTTFTP95), qc) },
		RequestRateQuery,
		UsageSamplesQuery,
		func(qc QueryContext) (string, error) {
			return AutoscalingQuery(inferencev1alpha1.AutoscalingQueueDepth, qc)
		},
	} {
		before, err := render(stable)
		if err != nil {
			t.Fatal(err)
		}
		after, err := render(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if before != after || strings.Contains(after, `model=`) {
			t.Fatalf("model update hid the stable variant: %s -> %s", before, after)
		}
	}
}

func TestResourceIdentitySeparatesRepeatedModels(t *testing.T) {
	queries := make(map[string]bool)
	for _, identity := range [][2]string{
		{testNamespace, testModelDeployment}, {testNamespace, "support"}, {"team-b", testModelDeployment},
	} {
		qc := canaryCtx
		qc.Namespace, qc.ModelDeployment = identity[0], identity[1]
		query, err := Query(builtin(inferencev1alpha1.MetricQueueDepth), qc)
		if err != nil {
			t.Fatal(err)
		}
		if queries[query] {
			t.Fatal("different resources rendered the same query")
		}
		queries[query] = true
	}
}

func TestPromDuration(t *testing.T) {
	t.Parallel()

	// time.Duration.String() produces "1m0s", which Prometheus rejects, and
	// truncates "1.5s" to "1s" — a difference that matters when the window is
	// being sized against a scrape interval.
	cases := map[time.Duration]string{
		60 * time.Second:        "60s",
		90 * time.Second:        "90s",
		2 * time.Minute:         "120s",
		1500 * time.Millisecond: "2s",
		0:                       "1s",
	}
	for d, want := range cases {
		if got := PromDuration(d); got != want {
			t.Errorf("PromDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestAutoscalingQueryIsFleetWide(t *testing.T) {
	t.Parallel()

	// Every other query in this package pins `variant`, because comparing a
	// canary against its primary is the point. This one must NOT: an autoscaler
	// decides how much capacity exists, and measuring one variant would size
	// the fleet from a fraction of its load that changes at every canary step.
	fleet := QueryContext{
		Namespace: testNamespace, ModelDeployment: testModelDeployment, Model: testModel, Window: testWindow,
	}

	cases := map[inferencev1alpha1.AutoscalingMetric]string{
		inferencev1alpha1.AutoscalingQueueDepth: `sum(avg_over_time(llmcp_inference_queue_depth` +
			`{namespace="team-a",model_deployment="chat"}[60s]))`,
		inferencev1alpha1.AutoscalingConcurrency: `sum(avg_over_time(llmcp_inference_requests_in_flight` +
			`{namespace="team-a",model_deployment="chat"}[60s]))`,
	}

	for metric, want := range cases {
		got, err := AutoscalingQuery(metric, fleet)
		if err != nil {
			t.Errorf("%s: %v", metric, err)
			continue
		}
		if got != want {
			t.Errorf("%s:\n got: %s\nwant: %s", metric, got, want)
		}
		if strings.Contains(got, "variant=") {
			t.Errorf("%s pins the variant label, which would size the fleet from part of its load: %s",
				metric, got)
		}
	}
}

func TestAutoscalingQueryAveragesBeforeSumming(t *testing.T) {
	t.Parallel()

	// Summing an instantaneous gauge would sample whichever microsecond each
	// scrape landed on. Queue depth is spiky by nature — it is the difference
	// between arrivals and a fixed number of decode slots — so it has to be
	// smoothed per pod before the fleet total is taken.
	got, err := AutoscalingQuery(inferencev1alpha1.AutoscalingQueueDepth,
		QueryContext{
			Namespace: testNamespace, ModelDeployment: testModelDeployment, Model: testModel, Window: testWindow,
		})
	if err != nil {
		t.Fatalf("AutoscalingQuery: %v", err)
	}
	if !strings.HasPrefix(got, "sum(avg_over_time(") {
		t.Errorf("query = %s, want the average taken inside the sum", got)
	}
}

func TestAutoscalingQueryRejectsAnUnknownMetric(t *testing.T) {
	t.Parallel()

	// Rendering an empty query would produce a Prometheus parse error at the
	// worst moment; refusing here surfaces it as a status message instead.
	if _, err := AutoscalingQuery("no-such-metric", canaryCtx); err == nil {
		t.Fatal("an unknown autoscaling metric must be rejected")
	}
}
