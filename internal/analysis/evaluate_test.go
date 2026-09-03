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
	"errors"
	"math"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// successMetricName names the min-bounded fixture metric.
const successMetricName = "success"

func q(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

// metricWithMax is a max-bounded metric shared by the evaluation and analyzer
// tests. It carries a raw query so that it is renderable as well as evaluable —
// Evaluate ignores the query, but the analyzer has to build one.
func metricWithMax(maxV string) inferencev1alpha1.AnalysisMetric {
	return inferencev1alpha1.AnalysisMetric{
		Name:           "ttft",
		Query:          trivialQuery,
		ThresholdRange: inferencev1alpha1.ThresholdRange{Max: q(maxV)},
	}
}

func sample(v float64) Sample { return Sample{Value: v, Count: 1} }

// TestEvaluateFullMatrix walks every shape a metric backend can return.
//
// Each row exists because some canary controller shipped without it and
// promoted a broken release. The NaN and empty-result rows in particular are
// the difference between a gate and a decoration: both compare FALSE against
// every threshold, so an unguarded `value > max` reports Pass for a canary that
// served nothing at all.
func TestEvaluateFullMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		metric inferencev1alpha1.AnalysisMetric
		obs    Observation
		want   inferencev1alpha1.Verdict
		msg    string
	}{
		{
			name:   "inside the range passes",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: sample(1.2)},
			want:   inferencev1alpha1.VerdictPass,
		},
		{
			name:   "outside the range fails",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: sample(2.0)},
			want:   inferencev1alpha1.VerdictFail,
		},
		{
			// Inclusive, because "at most 1.5" should accept exactly 1.5. An
			// exclusive bound makes the boundary case depend on floating-point
			// representation, which nobody intends to gate a release on.
			name:   "exactly at the maximum passes",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: sample(1.5)},
			want:   inferencev1alpha1.VerdictPass,
		},
		{
			name: "exactly at the minimum passes",
			metric: inferencev1alpha1.AnalysisMetric{
				Name:           successMetricName,
				ThresholdRange: inferencev1alpha1.ThresholdRange{Min: q("990m")},
			},
			obs:  Observation{Sample: sample(0.99)},
			want: inferencev1alpha1.VerdictPass,
		},
		{
			name: "below the minimum fails",
			metric: inferencev1alpha1.AnalysisMetric{
				Name:           successMetricName,
				ThresholdRange: inferencev1alpha1.ThresholdRange{Min: q("990m")},
			},
			obs:  Observation{Sample: sample(0.5)},
			want: inferencev1alpha1.VerdictFail,
		},
		{
			name: "inside a two-sided band passes",
			metric: inferencev1alpha1.AnalysisMetric{
				Name: "rate",
				ThresholdRange: inferencev1alpha1.ThresholdRange{
					Min: q("1"), Max: q("10"),
				},
			},
			obs:  Observation{Sample: sample(5)},
			want: inferencev1alpha1.VerdictPass,
		},
		{
			// A monitoring outage must never roll back a production deployment,
			// so this is Error and not Fail.
			name:   "a transport failure is an error, never a failure",
			metric: metricWithMax("1500m"),
			obs:    Observation{Err: errors.New("connection refused")},
			want:   inferencev1alpha1.VerdictError,
			msg:    "connection refused",
		},
		{
			// The single most damaging misreading available: an empty result is
			// the ABSENCE of a measurement, not a measurement of zero.
			name:   "an empty result is inconclusive, not a passing zero",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: Sample{Count: 0}},
			want:   inferencev1alpha1.VerdictInconclusive,
			msg:    "matched no series",
		},
		{
			name:   "multiple series is an error, not an arbitrary pick",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: Sample{Value: 1, Count: 4}},
			want:   inferencev1alpha1.VerdictError,
			msg:    "aggregate",
		},
		{
			// histogram_quantile over empty buckets. NaN compares FALSE against
			// every threshold, so without this branch it would report Pass.
			name:   "NaN is inconclusive",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: sample(math.NaN())},
			want:   inferencev1alpha1.VerdictInconclusive,
			msg:    LiteralNaN,
		},
		{
			name:   "positive infinity is inconclusive",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: sample(math.Inf(1))},
			want:   inferencev1alpha1.VerdictInconclusive,
			msg:    "infinity",
		},
		{
			name:   "negative infinity is inconclusive",
			metric: metricWithMax("1500m"),
			obs:    Observation{Sample: sample(math.Inf(-1))},
			want:   inferencev1alpha1.VerdictInconclusive,
			msg:    "infinity",
		},
		{
			// A genuine measurement of zero is NOT the same as no measurement,
			// and must pass a max-only threshold.
			name:   "a real zero passes a maximum threshold",
			metric: metricWithMax("50m"),
			obs:    Observation{Sample: sample(0)},
			want:   inferencev1alpha1.VerdictPass,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Evaluate(tc.metric, tc.obs)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (message: %s)", got.Verdict, tc.want, got.Message)
			}
			if tc.msg != "" && !strings.Contains(got.Message, tc.msg) {
				t.Errorf("message = %q, want it to mention %q", got.Message, tc.msg)
			}
			if got.Name != tc.metric.Name {
				t.Errorf("name = %q, want %q", got.Name, tc.metric.Name)
			}
			if got.Threshold == "" {
				t.Error("threshold was not restated; a status read later would not explain itself")
			}
		})
	}
}

func TestEvaluateRendersSpecialValues(t *testing.T) {
	t.Parallel()

	// Rendered as themselves rather than blanked: a status saying "NaN" tells
	// the reader exactly what happened, whereas an empty field reads like a bug
	// in the operator.
	for value, want := range map[float64]string{
		math.NaN():   "NaN",
		math.Inf(1):  "+Inf",
		math.Inf(-1): "-Inf",
		1.2345678901: "1.23457",
		0:            "0",
	} {
		got := Evaluate(metricWithMax("1500m"), Observation{Sample: sample(value)})
		if got.Value != want {
			t.Errorf("value %v rendered as %q, want %q", value, got.Value, want)
		}
	}
}

func TestEvaluateQuantityBoundaryIsExact(t *testing.T) {
	t.Parallel()

	// "1500m" must compare as exactly 1.5, not 1.4999999999999998. Going
	// through MilliValue rather than a float conversion is what makes a value
	// sitting precisely on the boundary land on the intended side of it.
	for _, spelling := range []string{"1500m", "1.5"} {
		got := Evaluate(metricWithMax(spelling), Observation{Sample: sample(1.5)})
		if got.Verdict != inferencev1alpha1.VerdictPass {
			t.Errorf("threshold %q against exactly 1.5: verdict = %q, want Pass",
				spelling, got.Verdict)
		}
	}
}

func TestRenderRangeExplainsItself(t *testing.T) {
	t.Parallel()

	cases := map[string]inferencev1alpha1.ThresholdRange{
		"<= 1500m":  {Max: q("1500m")},
		">= 990m":   {Min: q("990m")},
		"[1, 10]":   {Min: q("1"), Max: q("10")},
		"unbounded": {},
	}
	for want, r := range cases {
		if got := renderRange(r); got != want {
			t.Errorf("renderRange(%+v) = %q, want %q", r, got, want)
		}
	}
}

func TestInconclusiveAndErroredHelpers(t *testing.T) {
	t.Parallel()

	m := metricWithMax("1500m")

	inc := Inconclusive(m, "no traffic")
	if inc.Verdict != inferencev1alpha1.VerdictInconclusive || inc.Message != "no traffic" {
		t.Errorf("Inconclusive = %+v", inc)
	}
	if inc.Value != "" {
		t.Error("Inconclusive must not invent a value; an empty field is the honest answer")
	}

	e := Errored(m, "provider down")
	if e.Verdict != inferencev1alpha1.VerdictError || e.Message != "provider down" {
		t.Errorf("Errored = %+v", e)
	}
}
