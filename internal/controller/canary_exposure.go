package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/canary"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

const (
	observationAwaitingFreshWindow = "AwaitingFreshWindow"
	observationProviderUnavailable = "ProviderUnavailable"
	observationMissingSeries       = "MissingSeries"
	observationStaleSamples        = "StaleSamples"
	observationInsufficientTraffic = "InsufficientTraffic"
	observationMeasured            = "Measured"
)

// observeCanaryTraffic is deliberately outside the decision state machine.
// A missing observation affects only its display and retry timer.
func (r *ModelDeploymentReconciler) observeCanaryTraffic(
	ctx context.Context, md *inferencev1alpha1.ModelDeployment, plan canary.Plan,
	now time.Time, out *canary.Output,
) {
	if out.Canary == 0 || out.State.Revision == "" {
		return
	}
	out.Observation.Reason = observationAwaitingFreshWindow
	if out.State.AvailableSince.IsZero() || now.Before(out.State.AvailableSince.Add(plan.InitialDelay+plan.Window)) {
		return
	}
	prev := md.Status.Canary
	// An unusually long analysis interval must not keep a measurement visible
	// after its entire lookback has aged out.
	retention := min(plan.Interval, plan.Window)
	if prev != nil && prev.Revision == out.State.Revision && prev.ReadinessTarget == out.State.ReadinessTarget &&
		prev.ObservedAt != nil && prev.ObservationWindow != nil && prev.ObservationWindow.Duration == plan.Window &&
		!prev.ObservedAt.Time.Before(out.State.AvailableSince.Add(plan.InitialDelay+plan.Window)) &&
		!now.Before(prev.ObservedAt.Time) && now.Before(prev.ObservedAt.Add(retention)) {
		out.Observation = canary.TrafficObservation{
			Weight: prev.ObservedWeight, At: prev.ObservedAt.Time,
			Window: prev.ObservationWindow.Duration, Reason: prev.ObservationReason,
		}
		out.RequeueAfter = soonest(out.RequeueAfter, prev.ObservedAt.Add(retention).Sub(now))
		return
	}
	out.Observation = sampleCanaryTraffic(ctx, r.providerFor(md), md, now, plan.Window)
	out.RequeueAfter = soonest(out.RequeueAfter, retention)
}

func sampleCanaryTraffic(ctx context.Context, provider analysis.Provider, md *inferencev1alpha1.ModelDeployment,
	now time.Time, window time.Duration,
) canary.TrafficObservation {
	observation := canary.TrafficObservation{At: now, Window: window, Reason: observationProviderUnavailable}
	if provider == nil {
		return observation
	}
	var rates [2]float64
	for i, variant := range []string{"primary", "canary"} {
		// Identity, rather than the target model name, selects both variants:
		// the stable primary can legitimately be serving a different model.
		selector := fmt.Sprintf("%s{namespace=%s,model_deployment=%s,variant=%s}",
			llmcpmetrics.RequestsStartedTotal, strconv.Quote(md.Namespace), strconv.Quote(md.Name),
			strconv.Quote(variant))
		// @ fixes both selectors to one evaluation time even if network calls
		// straddle a scrape. timestamp() checks source freshness: the timestamp
		// of an aggregated rate result would only describe query evaluation.
		at := strconv.FormatFloat(float64(now.UnixMilli())/1000, 'f', 3, 64)
		fresh, err := provider.Query(ctx, fmt.Sprintf("min(timestamp(%s @ %s))", selector, at))
		if err != nil {
			return observation
		}
		if fresh.Count != 1 || math.IsNaN(fresh.Value) || math.IsInf(fresh.Value, 0) {
			observation.Reason = observationMissingSeries
			return observation
		}
		age := float64(now.UnixMilli())/1000 - fresh.Value
		if age < 0 || age > window.Seconds() {
			observation.Reason = observationStaleSamples
			return observation
		}
		rate, err := provider.Query(ctx, fmt.Sprintf("sum(rate(%s[%s] @ %s))", selector, analysis.PromDuration(window), at))
		if err != nil {
			return observation
		}
		if rate.Count != 1 || math.IsNaN(rate.Value) || math.IsInf(rate.Value, 0) || rate.Value < 0 {
			observation.Reason = observationMissingSeries
			return observation
		}
		rates[i] = rate.Value
	}
	total := rates[0] + rates[1]
	if math.IsInf(total, 0) || total <= 0 || total < md.Spec.Rollout.Canary.Analysis.ResolvedMinRequestRate() {
		observation.Reason = observationInsufficientTraffic
		return observation
	}
	weight := int32(math.Round(100 * (rates[1] / total)))
	observation.Weight, observation.Reason = &weight, observationMeasured
	return observation
}
