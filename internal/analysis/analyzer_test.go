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
	"context"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

func request(metrics ...inferencev1alpha1.AnalysisMetric) Request {
	return Request{
		Model:          testModel,
		Window:         testWindow,
		MinRequestRate: 0.5,
		Metrics:        metrics,
	}
}

// verdicts extracts the verdicts from a round's checks, for compact assertions.
func verdicts(checks []inferencev1alpha1.MetricCheck) []inferencev1alpha1.Verdict {
	out := make([]inferencev1alpha1.Verdict, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Verdict)
	}
	return out
}

func TestTrafficGatePassesWhenThereIsEnoughLoad(t *testing.T) {
	t.Parallel()

	p := NewScripted(
		Pass(5).For("llmcp_inference_requests_total"), // the traffic gate
		Pass(0.8), // the metric
	)
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), request(metricWithMax("1500m")))
	if got := verdicts(checks); len(got) != 1 || got[0] != inferencev1alpha1.VerdictPass {
		t.Fatalf("verdicts = %v, want [Pass]", got)
	}
}

// TestTrafficGateShortCircuitsEveryCheck is the "healthy because empty" guard.
//
// Gating the WHOLE round rather than each metric is what makes it robust. With
// no traffic, an error-rate query returns a clean 0 and would Pass on its own —
// so a rollout whose only gate is error rate would promote a canary that served
// nothing. The round-level gate removes that possibility instead of relying on
// the metric mix to preserve it.
func TestTrafficGateShortCircuitsEveryCheck(t *testing.T) {
	t.Parallel()

	p := NewScripted(Pass(0.1).For("llmcp_inference_requests_total"))
	a := &Analyzer{Provider: p}

	errRate := inferencev1alpha1.MetricErrorRate
	checks := a.Run(context.Background(), request(
		inferencev1alpha1.AnalysisMetric{
			Name:           "error-rate",
			Builtin:        &errRate,
			ThresholdRange: inferencev1alpha1.ThresholdRange{Max: q("50m")},
		},
	))

	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(checks))
	}
	if checks[0].Verdict != inferencev1alpha1.VerdictInconclusive {
		t.Fatalf("verdict = %q, want Inconclusive: a canary with no traffic has not been tested",
			checks[0].Verdict)
	}
	if !strings.Contains(checks[0].Message, "below the minimum") {
		t.Errorf("message = %q, want it to name the traffic floor", checks[0].Message)
	}

	// The metric itself must never have been queried: short-circuiting is the
	// point, and a query here would mean the gate is advisory.
	for _, query := range p.Queries {
		if strings.Contains(query, `code=~"5.."`) {
			t.Error("the error-rate query ran despite the traffic gate")
		}
	}
}

func TestTrafficGateReportsMissingSeriesDistinctly(t *testing.T) {
	t.Parallel()

	p := NewScripted(Empty().For("llmcp_inference_requests_total"))
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), request(metricWithMax("1500m")))
	if checks[0].Verdict != inferencev1alpha1.VerdictInconclusive {
		t.Fatalf("verdict = %q, want Inconclusive", checks[0].Verdict)
	}
	if !strings.Contains(checks[0].Message, "shim") {
		t.Errorf("message = %q; it should point at the two plausible causes "+
			"(no traffic, or the shim not being scraped)", checks[0].Message)
	}
}

func TestTrafficGateFailureIsAnErrorNotInconclusive(t *testing.T) {
	t.Parallel()

	// An unreachable provider says nothing about how much traffic the canary
	// received, so it must spend the ERROR budget — which resets — rather than
	// the inconclusive one.
	p := NewScripted(Fail(ErrProviderDown).For("llmcp_inference_requests_total"))
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), request(metricWithMax("1500m")))
	if checks[0].Verdict != inferencev1alpha1.VerdictError {
		t.Fatalf("verdict = %q, want Error", checks[0].Verdict)
	}
}

func TestTrafficGateIsSkippedWhenDisabled(t *testing.T) {
	t.Parallel()

	req := request(metricWithMax("1500m"))
	req.MinRequestRate = 0

	p := NewScripted(Pass(0.8))
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), req)
	if checks[0].Verdict != inferencev1alpha1.VerdictPass {
		t.Fatalf("verdict = %q, want Pass", checks[0].Verdict)
	}
	for _, query := range p.Queries {
		if query == RequestRateQuery(canaryCtx) {
			t.Error("the traffic gate query ran although minRequestRate is 0")
		}
	}
}

func TestCompareToPrimaryBuildsARatio(t *testing.T) {
	t.Parallel()

	// The ratio is the important default: an absolute latency threshold has to
	// be re-tuned per model and per node type, and a stale one fires on a busy
	// afternoon rather than on a bad release.
	ttft := inferencev1alpha1.MetricTTFTP95
	m := inferencev1alpha1.AnalysisMetric{
		Name:             "ttft-vs-primary",
		Builtin:          &ttft,
		CompareToPrimary: ptr.To(true),
		ThresholdRange:   inferencev1alpha1.ThresholdRange{Max: q("1500m")},
	}

	p := NewScripted(
		Pass(5).For(llmcpmetrics.RequestsTotal),
		Pass(1.2),
	)
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), request(m))
	if checks[0].Verdict != inferencev1alpha1.VerdictPass {
		t.Fatalf("verdict = %q, want Pass", checks[0].Verdict)
	}

	var ratio string
	for _, query := range p.Queries {
		if strings.Contains(query, "clamp_min") && strings.Contains(query, "ttft") {
			ratio = query
		}
	}
	if ratio == "" {
		t.Fatal("no ratio query was issued")
	}
	if !strings.Contains(ratio, `variant="canary"`) || !strings.Contains(ratio, `variant="primary"`) {
		t.Errorf("the ratio does not compare the two variants: %s", ratio)
	}
}

func TestEachMetricIsEvaluatedIndependently(t *testing.T) {
	t.Parallel()

	p := NewScripted(
		Pass(5).For(llmcpmetrics.RequestsTotal),
		Pass(0.8),  // first metric, inside its band
		Pass(99.0), // second metric, outside its band
	)
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), request(
		inferencev1alpha1.AnalysisMetric{
			Name: "a", Query: trivialQuery,
			ThresholdRange: inferencev1alpha1.ThresholdRange{Max: q("1")},
		},
		inferencev1alpha1.AnalysisMetric{
			Name: "b", Query: "vector(2)",
			ThresholdRange: inferencev1alpha1.ThresholdRange{Max: q("1")},
		},
	))

	got := verdicts(checks)
	want := []inferencev1alpha1.Verdict{inferencev1alpha1.VerdictPass, inferencev1alpha1.VerdictFail}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("verdicts = %v, want %v", got, want)
	}
	if checks[0].Name != "a" || checks[1].Name != "b" {
		t.Errorf("checks are not in spec order: %s, %s", checks[0].Name, checks[1].Name)
	}
}

func TestUnrenderableMetricIsAnError(t *testing.T) {
	t.Parallel()

	// A metric the operator cannot render is a spec problem, not evidence about
	// the canary — so it must not spend the rollback budget.
	p := NewScripted(Pass(5).For(llmcpmetrics.RequestsTotal))
	a := &Analyzer{Provider: p}

	checks := a.Run(context.Background(), request(
		inferencev1alpha1.AnalysisMetric{Name: "broken"},
	))
	if checks[0].Verdict != inferencev1alpha1.VerdictError {
		t.Fatalf("verdict = %q, want Error", checks[0].Verdict)
	}
}

func TestScriptedProviderExhaustionIsLoud(t *testing.T) {
	t.Parallel()

	// A script that ran out is a test bug. Reporting it as a provider error
	// would let the canary logic absorb it and carry on, turning a broken test
	// into a passing one.
	p := NewScripted()
	if _, err := p.Query(context.Background(), "anything"); err == nil {
		t.Fatal("an exhausted script must report an error")
	}
}

func TestScriptedProviderRepeat(t *testing.T) {
	t.Parallel()

	p := NewScripted(Pass(1))
	p.Repeat = true

	for i := range 5 {
		s, err := p.Query(context.Background(), "q")
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		if s.Value != 1 {
			t.Fatalf("query %d: value = %v", i, s.Value)
		}
	}
}

func TestScriptedProviderRemaining(t *testing.T) {
	t.Parallel()

	// A test that finishes with steps left over expected more analysis rounds
	// than actually ran — the same class of bug as running off the end, and far
	// easier to miss because nothing fails.
	p := NewScripted(Pass(1), Pass(2), Pass(3).For("only-this"))
	if got := p.Remaining(); got != 2 {
		t.Fatalf("Remaining = %d before any query, want 2 (matched steps do not count)", got)
	}
	_, _ = p.Query(context.Background(), "q")
	if got := p.Remaining(); got != 1 {
		t.Fatalf("Remaining = %d after one query, want 1", got)
	}
}
