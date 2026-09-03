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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
)

// Operator metrics, describing the control plane's DECISIONS rather than the
// workload's behaviour.
//
// They are registered on controller-runtime's shared registry, which the
// manager already serves on the metrics endpoint alongside
// controller_runtime_* and workqueue_*. Standing up a second registry and a
// second listener would mean a second scrape target, a second ServiceMonitor
// and a second set of RBAC for no gain.
//
// # Why these exist at all
//
// A rollback is the single most important thing this operator does, and until
// now it was observable only as a Kubernetes event — which expires, is
// namespace-scoped, and cannot be graphed. A counter can be. The canary
// dashboard's annotated rollback marker, which is the money shot of the whole
// project, is sourced from CanaryRollbacks.
//
// # Cardinality
//
// Every series is keyed by (model, namespace, name), so the count is bounded by
// the number of ModelDeployments in the cluster. The gauges are DELETED when a
// ModelDeployment goes away — see ForgetNamespacedName — because a gauge for a
// resource that no longer exists is not merely a leak, it keeps reporting a
// canary weight for a rollout that ended.
var (
	// CanaryPromotions counts canaries that became the primary.
	CanaryPromotions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: llmcpmetrics.CanaryPromotionsTotal,
		Help: "Canaries promoted to primary, by ModelDeployment.",
	}, resourceLabels)

	// CanaryRollbacks counts canaries torn down, by cause.
	CanaryRollbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: llmcpmetrics.CanaryRollbacksTotal,
		Help: "Canaries rolled back, by ModelDeployment and cause.",
	}, resourceLabelsWith(llmcpmetrics.LabelReason))

	// CanaryCurrentWeight is the realised traffic share.
	CanaryCurrentWeight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: llmcpmetrics.CanaryCurrentWeight,
		Help: "Traffic share the canary is actually receiving, after quantisation.",
	}, resourceLabels)

	// CanaryDesiredWeight is the share the current ladder step asked for.
	CanaryDesiredWeight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: llmcpmetrics.CanaryDesiredWeight,
		Help: "Traffic share the current canary step requested, before quantisation.",
	}, resourceLabels)

	// CanaryFailedChecks is the failure count against the threshold.
	CanaryFailedChecks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: llmcpmetrics.CanaryFailedChecks,
		Help: "Analysis rounds that failed a threshold in the current canary.",
	}, resourceLabels)

	// AnalysisVerdicts counts rounds by outcome.
	AnalysisVerdicts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: llmcpmetrics.AnalysisVerdictsTotal,
		Help: "Canary analysis rounds by verdict: Pass, Fail, Inconclusive or Error.",
	}, resourceLabelsWith(llmcpmetrics.LabelVerdict))

	// AnalysisCheckDuration is how long one analysis round took.
	AnalysisCheckDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: llmcpmetrics.AnalysisCheckDurationSeconds,
		Help: "Wall time of one canary analysis round, including every metric query.",
		// Sub-millisecond to ten seconds. The lower end is where a scripted
		// provider in a test lands; the upper end is a Prometheus that is
		// struggling, which is worth being able to see before it becomes a
		// timeout.
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	// AutoscaleDesiredReplicas is the autoscaler's most recent decision.
	AutoscaleDesiredReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: llmcpmetrics.AutoscaleDesiredReplicas,
		Help: "Replica count the built-in autoscaler most recently asked for.",
	}, resourceLabels)
)

// resourceLabels identify the ModelDeployment a series describes.
//
// All three, not just `model`: a user-chosen model name can legitimately be
// shared by two ModelDeployments, and a resource name is unique only within a
// namespace. A dashboard grouping by `model` alone would silently merge a
// staging and a production rollout of the same model into one series.
var resourceLabels = []string{
	llmcpmetrics.LabelModel,
	llmcpmetrics.LabelNamespace,
	llmcpmetrics.LabelName,
}

// resourceLabelsWith returns the resource labels plus one more.
func resourceLabelsWith(extra string) []string {
	out := make([]string, 0, len(resourceLabels)+1)
	out = append(out, resourceLabels...)
	return append(out, extra)
}

// ResourceKey identifies one ModelDeployment's series.
type ResourceKey struct {
	Model     string
	Namespace string
	Name      string
}

// labels renders the key as Prometheus labels.
func (k ResourceKey) labels() prometheus.Labels {
	return prometheus.Labels{
		llmcpmetrics.LabelModel:     k.Model,
		llmcpmetrics.LabelNamespace: k.Namespace,
		llmcpmetrics.LabelName:      k.Name,
	}
}

func init() {
	// MustRegister on the shared registry. A duplicate registration is a
	// programming error that can only be introduced at compile time, and it
	// would otherwise surface as a silently missing metric.
	ctrlmetrics.Registry.MustRegister(
		CanaryPromotions,
		CanaryRollbacks,
		CanaryCurrentWeight,
		CanaryDesiredWeight,
		CanaryFailedChecks,
		AnalysisVerdicts,
		AnalysisCheckDuration,
		AutoscaleDesiredReplicas,
	)
}

// RecordCanaryPromotion counts a promotion.
func RecordCanaryPromotion(k ResourceKey) {
	CanaryPromotions.With(k.labels()).Inc()
}

// RecordCanaryRollback counts a rollback, keyed by cause.
func RecordCanaryRollback(k ResourceKey, reason string) {
	l := k.labels()
	l[llmcpmetrics.LabelReason] = reason
	CanaryRollbacks.With(l).Inc()
}

// RecordCanaryState publishes the current weights and failure count.
//
// Called on every reconcile with a canary in flight, so the gauges reflect the
// present rather than the last transition — which is the difference between a
// weight graph that steps and one that is a row of disconnected points.
func RecordCanaryState(k ResourceKey, desiredWeight, currentWeight, failedChecks int32) {
	l := k.labels()
	CanaryDesiredWeight.With(l).Set(float64(desiredWeight))
	CanaryCurrentWeight.With(l).Set(float64(currentWeight))
	CanaryFailedChecks.With(l).Set(float64(failedChecks))
}

// RecordAnalysisRound counts one round's verdict and its duration.
func RecordAnalysisRound(k ResourceKey, verdict string, took time.Duration) {
	l := k.labels()
	l[llmcpmetrics.LabelVerdict] = verdict
	AnalysisVerdicts.With(l).Inc()
	AnalysisCheckDuration.Observe(took.Seconds())
}

// RecordAutoscaleDecision publishes the autoscaler's desired replica count.
func RecordAutoscaleDecision(k ResourceKey, replicas int32) {
	AutoscaleDesiredReplicas.With(k.labels()).Set(float64(replicas))
}

// ForgetCanary drops only the in-flight canary gauges, leaving the rest.
//
// Called when a rollout ends while the ModelDeployment lives on. Without it the
// weight gauge would sit at its final value forever, and a dashboard would show
// a canary permanently carrying 50% of traffic long after it was promoted.
func ForgetCanary(k ResourceKey) {
	l := k.labels()
	CanaryCurrentWeight.Delete(l)
	CanaryDesiredWeight.Delete(l)
	CanaryFailedChecks.Delete(l)
}

// ForgetNamespacedName drops every gauge for a ModelDeployment whose model
// label is no longer known.
//
// This is the deletion path: by the time the controller learns a resource is
// gone, the object has been removed from the API server and its
// spec.model.name with it. Deleting by exact label set is therefore impossible,
// so the partial match is the only option — which is exactly what
// DeletePartialMatch exists for.
//
// Counters are deliberately left alone. A promotion that happened did happen,
// and removing its counter would make increase() over a window spanning the
// deletion report a decrease, which Prometheus reads as a counter reset and
// silently mis-attributes.
func ForgetNamespacedName(namespace, name string) {
	partial := prometheus.Labels{
		llmcpmetrics.LabelNamespace: namespace,
		llmcpmetrics.LabelName:      name,
	}
	CanaryCurrentWeight.DeletePartialMatch(partial)
	CanaryDesiredWeight.DeletePartialMatch(partial)
	CanaryFailedChecks.DeletePartialMatch(partial)
	AutoscaleDesiredReplicas.DeletePartialMatch(partial)
}
