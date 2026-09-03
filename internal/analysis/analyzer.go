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
	"fmt"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// Request is one analysis round's inputs.
type Request struct {
	// Model is spec.model.name, the `model` label on every series.
	Model string

	// Window is the PromQL lookback, already rendered ("60s").
	Window string

	// MinRequestRate is the traffic floor, in requests per second, below which
	// the round is Inconclusive.
	MinRequestRate float64

	// Metrics are the checks to run.
	Metrics []inferencev1alpha1.AnalysisMetric
}

// Analyzer runs one analysis round against a Provider.
//
// It is the only stateful piece of this package, and its state is one field: a
// provider. Everything it does is a straight-line composition of the pure
// functions around it — Query renders, Provider fetches, Evaluate judges — so
// the only behaviour that lives HERE is the ordering, which is where the
// traffic gate belongs.
type Analyzer struct {
	Provider Provider
}

// Run evaluates every metric and returns the per-check results.
//
// # The traffic gate runs first, and short-circuits
//
// Before any metric is queried, the canary's request rate is checked against
// MinRequestRate. If the canary is receiving too little traffic, EVERY check is
// reported Inconclusive without being run.
//
// Doing this first rather than per-metric is what makes the gate meaningful. A
// canary with no traffic produces an error rate of 0 and no latency samples at
// all — so a per-metric evaluation would report error-rate as a clean Pass and
// latency as Inconclusive, the round would aggregate to Inconclusive, and the
// distinction would happen to work. It would stop working the moment somebody
// configured a rollout whose only gate is error rate, at which point the canary
// passes every check by serving nothing. Gating the whole round removes that
// possibility rather than relying on the metric mix to preserve it.
//
// A gate query that ERRORS is reported as Error, not Inconclusive: an
// unreachable provider says nothing about how much traffic the canary received.
func (a *Analyzer) Run(ctx context.Context, req Request) []inferencev1alpha1.MetricCheck {
	canaryCtx := QueryContext{
		Model:   req.Model,
		Variant: string(inferencev1alpha1.VariantCanary),
		Window:  req.Window,
	}
	primaryCtx := QueryContext{
		Model:   req.Model,
		Variant: string(inferencev1alpha1.VariantPrimary),
		Window:  req.Window,
	}

	if checks, gated := a.gateOnTraffic(ctx, req, canaryCtx); gated {
		return checks
	}

	checks := make([]inferencev1alpha1.MetricCheck, 0, len(req.Metrics))
	for i := range req.Metrics {
		checks = append(checks, a.runOne(ctx, req.Metrics[i], canaryCtx, primaryCtx))
	}
	return checks
}

// gateOnTraffic applies the minimum-request-rate precondition.
func (a *Analyzer) gateOnTraffic(
	ctx context.Context,
	req Request,
	canaryCtx QueryContext,
) ([]inferencev1alpha1.MetricCheck, bool) {
	if req.MinRequestRate <= 0 {
		return nil, false
	}

	sample, err := a.Provider.Query(ctx, RequestRateQuery(canaryCtx))
	if err != nil {
		return allChecks(req.Metrics, func(m inferencev1alpha1.AnalysisMetric) inferencev1alpha1.MetricCheck {
			return Errored(m, "could not measure the canary's request rate: "+err.Error())
		}), true
	}

	if sample.Count == 0 {
		return allChecks(req.Metrics, func(m inferencev1alpha1.AnalysisMetric) inferencev1alpha1.MetricCheck {
			return Inconclusive(m, "the canary has produced no request metrics at all; "+
				"either it is receiving no traffic or its shim is not being scraped")
		}), true
	}

	if sample.Count == 1 && sample.Value < req.MinRequestRate {
		return allChecks(req.Metrics, func(m inferencev1alpha1.AnalysisMetric) inferencev1alpha1.MetricCheck {
			return Inconclusive(m, fmt.Sprintf(
				"the canary is receiving %.3g req/s, below the minimum of %.3g req/s required to judge it",
				sample.Value, req.MinRequestRate))
		}), true
	}

	return nil, false
}

// runOne evaluates a single metric.
func (a *Analyzer) runOne(
	ctx context.Context,
	m inferencev1alpha1.AnalysisMetric,
	canaryCtx, primaryCtx QueryContext,
) inferencev1alpha1.MetricCheck {
	canaryQuery, err := Query(m, canaryCtx)
	if err != nil {
		// A metric the operator cannot render is a spec problem, not a provider
		// problem — but it is reported as Error rather than Fail all the same,
		// because it is still not evidence about the canary, and Fail is the
		// only verdict that spends the rollback budget.
		return Errored(m, "building the query: "+err.Error())
	}

	query := canaryQuery
	if m.CompareToPrimaryEnabled() {
		primaryQuery, perr := Query(m, primaryCtx)
		if perr != nil {
			return Errored(m, "building the primary comparison query: "+perr.Error())
		}
		query = RatioQuery(canaryQuery, primaryQuery)
	}

	sample, qerr := a.Provider.Query(ctx, query)
	return Evaluate(m, Observation{Sample: sample, Err: qerr})
}

// allChecks applies build to every metric.
func allChecks(
	metrics []inferencev1alpha1.AnalysisMetric,
	build func(inferencev1alpha1.AnalysisMetric) inferencev1alpha1.MetricCheck,
) []inferencev1alpha1.MetricCheck {
	out := make([]inferencev1alpha1.MetricCheck, 0, len(metrics))
	for i := range metrics {
		out = append(out, build(metrics[i]))
	}
	return out
}
