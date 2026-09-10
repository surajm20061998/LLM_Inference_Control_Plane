package controller

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/canary"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/test/helpers"
)

const exposureCandidateUID = "candidate"

func TestCanaryExposureEvidence(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 123000000, time.UTC)
	window := time.Minute
	stamp := float64(now.UnixMilli()) / 1000
	for _, tc := range []struct {
		name   string
		rates  [2]float64
		ages   [2]float64
		broken string
		reason string
		weight *int32
	}{
		{name: "one quarter of starts", rates: [2]float64{3, 1}, reason: observationMeasured, weight: exposureWeight(25)},
		{name: "present zero canary", rates: [2]float64{3, 0}, reason: observationMeasured, weight: exposureWeight(0)},
		{name: "present zero primary", rates: [2]float64{0, 3}, reason: observationMeasured, weight: exposureWeight(100)},
		{name: "rounding", rates: [2]float64{2, 1}, reason: observationMeasured, weight: exposureWeight(33)},
		{name: "no starts", reason: observationInsufficientTraffic},
		{name: "below configured floor", rates: [2]float64{0.1, 0.1}, reason: observationInsufficientTraffic},
		{name: "missing canary", rates: [2]float64{3, 1}, broken: "missing-canary", reason: observationMissingSeries},
		{name: "missing primary rate", rates: [2]float64{3, 1}, broken: "missing-rate", reason: observationMissingSeries},
		{name: "multiple rates", rates: [2]float64{3, 1}, broken: "multiple-rates", reason: observationMissingSeries},
		{name: "stale primary fresh canary", rates: [2]float64{3, 1}, ages: [2]float64{61, 0}, reason: observationStaleSamples},
		{name: "fresh primary stale canary", rates: [2]float64{3, 1}, ages: [2]float64{0, 61}, reason: observationStaleSamples},
		{name: "asymmetric fresh scrape times", rates: [2]float64{3, 1}, ages: [2]float64{10, 20}, reason: observationMeasured, weight: exposureWeight(25)},
		{name: "future timestamp", rates: [2]float64{3, 1}, ages: [2]float64{0, -1}, reason: observationStaleSamples},
		{name: "invalid timestamp", rates: [2]float64{3, 1}, ages: [2]float64{math.NaN(), 0}, reason: observationMissingSeries},
		{name: "negative rate", rates: [2]float64{-1, 2}, reason: observationMissingSeries},
		{name: "NaN rate", rates: [2]float64{math.NaN(), 2}, reason: observationMissingSeries},
		{name: "infinite rate", rates: [2]float64{1, math.Inf(1)}, reason: observationMissingSeries},
		{name: "overflowing total", rates: [2]float64{math.MaxFloat64, math.MaxFloat64}, reason: observationInsufficientTraffic},
		{name: "timestamp query failure", broken: "fresh-error", reason: observationProviderUnavailable},
		{name: "rate query failure", broken: "rate-error", reason: observationProviderUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := helpers.NewModelDeployment("serving", "tenant-a", helpers.WithCanary())
			provider := analysis.ProviderFunc(func(_ context.Context, query string) (analysis.Sample, error) {
				for _, required := range []string{llmcpmetrics.RequestsStartedTotal, `namespace="tenant-a"`, `model_deployment="serving"`, "@ " + strconv.FormatFloat(stamp, 'f', 3, 64)} {
					if !strings.Contains(query, required) {
						t.Errorf("missing %q in %s", required, query)
					}
				}
				if strings.Contains(query, "model=") {
					t.Fatal("target model name must not exclude the stable model from exposure")
				}
				i := 0
				if strings.Contains(query, `variant="canary"`) {
					i = 1
				} else if !strings.Contains(query, `variant="primary"`) {
					t.Fatalf("unscoped variant: %s", query)
				}
				if strings.HasPrefix(query, "min(timestamp(") {
					if tc.broken == "fresh-error" {
						return analysis.Sample{}, errors.New("unreachable")
					}
					if tc.broken == "missing-canary" && i == 1 {
						return analysis.Sample{}, nil
					}
					return analysis.Sample{Count: 1, Value: stamp - tc.ages[i]}, nil
				}
				if !strings.Contains(query, "[60s]") || !strings.HasPrefix(query, "sum(rate(") {
					t.Fatalf("invalid rate lookback: %s", query)
				}
				switch tc.broken {
				case "rate-error":
					return analysis.Sample{}, errors.New("unreachable")
				case "missing-rate":
					return analysis.Sample{}, nil
				case "multiple-rates":
					return analysis.Sample{Count: 2, Value: 3}, nil
				}
				return analysis.Sample{Count: 1, Value: tc.rates[i]}, nil
			})
			got := sampleCanaryTraffic(context.Background(), provider, md, now, window)
			if got.Reason != tc.reason || !reflect.DeepEqual(got.Weight, tc.weight) || got.At != now || got.Window != window {
				t.Fatalf("unexpected observation: %+v, expected reason %s weight %v", got, tc.reason, tc.weight)
			}
		})
	}
}

func TestCanaryExposureCadenceAndInvalidation(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	plan := canary.Plan{Interval: 30 * time.Second, Window: time.Minute, InitialDelay: time.Minute}
	target := inferencev1alpha1.CanaryReadinessTarget{
		Step: 1, TotalReplicas: 4, CanaryReplicas: 2, DeploymentUID: exposureCandidateUID, Generation: 2,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*inferencev1alpha1.ModelDeployment, *canary.Output, *canary.Plan)
		calls  int
		reason string
		weight *int32
	}{
		{name: "cached between attempts", calls: 0, reason: observationMeasured, weight: exposureWeight(25)},
		{name: "candidate changes", mutate: func(_ *inferencev1alpha1.ModelDeployment, out *canary.Output, _ *canary.Plan) {
			out.State.Revision = "replacement"
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "rung target changes", mutate: func(_ *inferencev1alpha1.ModelDeployment, out *canary.Output, _ *canary.Plan) {
			out.State.ReadinessTarget.Step++
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "readiness lost", mutate: func(_ *inferencev1alpha1.ModelDeployment, out *canary.Output, _ *canary.Plan) {
			out.State.AvailableSince = time.Time{}
		}, reason: observationAwaitingFreshWindow},
		{name: "new warmup", mutate: func(_ *inferencev1alpha1.ModelDeployment, out *canary.Output, _ *canary.Plan) {
			out.State.AvailableSince = now.Add(-time.Minute)
		}, reason: observationAwaitingFreshWindow},
		{name: "previous evidence predates fresh warmup", mutate: func(md *inferencev1alpha1.ModelDeployment, out *canary.Output, _ *canary.Plan) {
			out.State.AvailableSince = now.Add(-2 * time.Minute)
			md.Status.Canary.ObservedAt = &metav1.Time{Time: now.Add(-time.Second)}
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "query failure expires old weight", mutate: func(md *inferencev1alpha1.ModelDeployment, _ *canary.Output, _ *canary.Plan) {
			md.Status.Canary.ObservedAt = &metav1.Time{Time: now.Add(-30 * time.Second)}
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "lookback changed", mutate: func(_ *inferencev1alpha1.ModelDeployment, _ *canary.Output, p *canary.Plan) {
			p.Window = 2 * time.Minute
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "long interval cannot extend measurement lifetime", mutate: func(md *inferencev1alpha1.ModelDeployment, _ *canary.Output, p *canary.Plan) {
			p.Interval = 5 * time.Minute
			md.Status.Canary.ObservedAt = &metav1.Time{Time: now.Add(-time.Minute)}
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "future attempt is invalid", mutate: func(md *inferencev1alpha1.ModelDeployment, _ *canary.Output, _ *canary.Plan) {
			md.Status.Canary.ObservedAt = &metav1.Time{Time: now.Add(time.Second)}
		}, calls: 1, reason: observationProviderUnavailable},
		{name: "canary removed", mutate: func(_ *inferencev1alpha1.ModelDeployment, out *canary.Output, _ *canary.Plan) { out.Canary = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan
			md := helpers.NewModelDeployment("serving", "tenant-a", helpers.WithCanary())
			md.Status.Canary = &inferencev1alpha1.CanaryStatus{Revision: exposureCandidateUID, ReadinessTarget: target,
				ObservedWeight: exposureWeight(25), ObservedAt: &metav1.Time{Time: now.Add(-time.Second)},
				ObservationWindow: &metav1.Duration{Duration: plan.Window}, ObservationReason: observationMeasured}
			out := canary.Output{Canary: 2, State: canary.State{Revision: exposureCandidateUID, ReadinessTarget: target,
				AvailableSince: now.Add(-10 * time.Minute), StartedAt: now.Add(-time.Hour)}}
			out.State.SetCounters(1, 2, 3)
			if tc.mutate != nil {
				tc.mutate(md, &out, &p)
			}
			before := out.State
			calls := 0
			r := &ModelDeploymentReconciler{Provider: analysis.ProviderFunc(func(context.Context, string) (analysis.Sample, error) {
				calls++
				return analysis.Sample{}, errors.New("query failed")
			})}
			r.observeCanaryTraffic(context.Background(), md, p, now, &out)
			if calls != tc.calls || out.Observation.Reason != tc.reason || !reflect.DeepEqual(out.Observation.Weight, tc.weight) {
				t.Fatalf("calls=%d, observation=%+v", calls, out.Observation)
			}
			if !reflect.DeepEqual(out.State, before) {
				t.Fatal("display-only observation changed rollout state or verdict budgets")
			}
			status := canaryStatusFrom(out, nil, md.Status.Canary)
			if status == nil || !reflect.DeepEqual(status.ObservedWeight, tc.weight) || status.ObservationReason != tc.reason {
				t.Fatalf("status retained stale evidence: %+v", status)
			}
			if out.Observation.At.IsZero() && (status.ObservedAt != nil || status.ObservationWindow != nil) {
				t.Fatal("invalidated evidence retained old timestamps")
			}
		})
	}
}

func exposureWeight(v int32) *int32 { return &v }
