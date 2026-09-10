package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/analysis"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

// metricsScopeReady is the one-time handshake for each candidate's metric
// schema. Old shims cannot satisfy it through unscoped series. While it is
// pending, a caller holds without spending measured verdict budgets.
func (r *ModelDeploymentReconciler) metricsScopeReady(ctx context.Context, provider analysis.Provider,
	md *inferencev1alpha1.ModelDeployment, window time.Duration, primary, candidate int32,
) (bool, string) {
	if provider == nil || window <= 0 || md.Namespace == "" || md.Name == "" {
		return false, "Waiting for a configured provider and resource-scoped shim metrics"
	}
	now := r.now()
	at := strconv.FormatFloat(float64(now.UnixMilli())/1000, 'f', 3, 64)
	lookback := analysis.PromDuration(window)
	for _, variant := range []struct {
		name     string
		replicas int32
	}{{"primary", primary}, {"canary", candidate}} {
		if variant.replicas <= 0 {
			continue
		}
		sel := fmt.Sprintf(`%s{namespace=%q,model_deployment=%q,variant=%q}`, llmcpmetrics.ShimInfo, md.Namespace, md.Name, variant.name)
		// Match the same complete label set at both ends of the window, and
		// reject stale current samples. The @ modifier fixes one observation
		// time across both variant queries. Model names can change in a rollout.
		query := fmt.Sprintf(`count((%s @ %s == 1) and (%s @ %s offset %s == 1) and (timestamp(%s @ %s) >= %s))`,
			sel, at, sel, at, lookback, sel, at, strconv.FormatFloat(float64(now.Add(-window).UnixMilli())/1000, 'f', 3, 64))
		sample, err := provider.Query(ctx, query)
		if err != nil {
			return false, "Could not verify resource-scoped shim metrics; check provider availability and upgrade pinned old shim images to a controller-compatible version"
		}
		if sample.Count != 1 || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) || sample.Value < float64(variant.replicas) {
			return false, fmt.Sprintf("Waiting for %d %s shims to expose namespace/model_deployment identity over a full %s window; upgrade pinned old shim images", variant.replicas, variant.name, lookback)
		}
	}
	return true, "Resource-scoped metric schema is ready"
}
