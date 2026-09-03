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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObservabilitySpec configures what the operator publishes about a
// ModelDeployment for Prometheus and Grafana to consume.
//
// Every knob here is optional and enabled by default. That is deliberate: the
// metrics this produces are not decoration, they are the inputs canary analysis
// and autoscaling read. A ModelDeployment whose observability was accidentally
// off would still serve traffic, but every rollout decision made about it would
// be made blind — so opting OUT is the choice that has to be explicit.
type ObservabilitySpec struct {
	// ServiceMonitor configures the prometheus-operator ServiceMonitor the
	// controller creates for this ModelDeployment.
	//
	// If the prometheus-operator CRDs are not installed in the cluster, no
	// ServiceMonitor is created and the MetricsRegistered condition explains
	// why. That is reported as a fact, not an error: a cluster without
	// Prometheus is a legitimate configuration, and failing the reconcile over
	// it would make the whole resource unusable.
	//
	// +optional
	ServiceMonitor *ServiceMonitorSpec `json:"serviceMonitor,omitempty"`

	// PrometheusRule configures the recording rules and burn-rate alerts the
	// controller creates.
	// +optional
	PrometheusRule *PrometheusRuleSpec `json:"prometheusRule,omitempty"`

	// SLO defines the objectives those alerts are written against.
	// +optional
	SLO *SLOSpec `json:"slo,omitempty"`
}

// PrometheusRuleSpec configures the generated recording and alerting rules.
type PrometheusRuleSpec struct {
	// Enabled creates the PrometheusRule. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Labels are added to the PrometheusRule's metadata, for a Prometheus whose
	// ruleSelector is not empty. Same escape hatch, and same silent failure
	// mode, as ServiceMonitorSpec.Labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// LongWindowAlerts emits the slow-burn tier as well: 3x over 1d and 1x over
	// 3d, the two rungs of the Google SRE ladder that catch a budget being
	// consumed steadily rather than suddenly.
	//
	// Off by default, and the reason is honesty rather than laziness. A demo
	// cluster is minutes old, so a 3d window has no data and every panel and
	// alert built on it shows "No Data" — which looks worse than not having
	// them and trains a viewer to ignore the alert list. Being explicit that
	// the SLO window is compressed for the demo is more credible than shipping
	// rules that cannot evaluate.
	//
	// Turn this on for a real deployment, where the windows mean what they say.
	//
	// +optional
	// +kubebuilder:default=false
	LongWindowAlerts *bool `json:"longWindowAlerts,omitempty"`
}

// SLOSpec declares the service level objectives alerts are written against.
//
// # Why TTFT and not end-to-end latency
//
// End-to-end request duration is a bad primary SLI for a language model,
// because it scales with how many tokens the caller asked for. A p95 over mixed
// traffic measures the request MIX as much as the server, so it moves when a
// client changes its max_tokens and stays flat through a genuine regression that
// happens to coincide with shorter prompts. An SLO built on it burns budget for
// reasons the service cannot act on.
//
// Time to first token is length-independent: it is how long the user waits
// before anything happens. Time per output token is the other half, also
// length-independent. That is exactly why OpenTelemetry's gen_ai semantic
// conventions define the two separately rather than shipping one latency
// metric, and it is why this operator's SLIs are TTFT and availability.
type SLOSpec struct {
	// TTFTThreshold is the latency a request must beat to count as good.
	//
	// It is SNAPPED to the nearest histogram bucket boundary at or above the
	// value, and the snapped figure is what the rules use. That is not a
	// rounding convenience: a latency SLI is a ratio over the bucket at an
	// exact `le` label, Prometheus matches `le` as a string, and a boundary the
	// histogram does not have returns an empty vector rather than an error. The
	// recording rule would then produce nothing and the burn-rate alert would
	// never fire — an SLO that cannot alert, which is worse than none because
	// it is believed.
	//
	// +optional
	// +kubebuilder:default="1500m"
	TTFTThreshold *resource.Quantity `json:"ttftThreshold,omitempty"`

	// TTFTObjective is the fraction of requests that must beat TTFTThreshold,
	// as a value in (0, 1).
	//
	// 0.99 means a 1% error budget. Note the budget is what the burn-rate
	// alerts are scaled against, so a stricter objective makes every alert more
	// sensitive rather than merely raising a bar.
	//
	// +optional
	// +kubebuilder:default="990m"
	TTFTObjective *resource.Quantity `json:"ttftObjective,omitempty"`

	// AvailabilityObjective is the fraction of requests that must not fail with
	// a 5xx.
	//
	// 5xx only: a 4xx is the caller's fault — a malformed prompt, a model name
	// that does not exist, a context overflow — and counting one against the
	// service would let a single misbehaving client burn an error budget the
	// service is not responsible for.
	//
	// +optional
	// +kubebuilder:default="995m"
	AvailabilityObjective *resource.Quantity `json:"availabilityObjective,omitempty"`
}

// PrometheusRuleEnabled reports whether the rules should be created.
func (o *ObservabilitySpec) PrometheusRuleEnabled() bool {
	if o == nil || o.PrometheusRule == nil || o.PrometheusRule.Enabled == nil {
		return true
	}
	return *o.PrometheusRule.Enabled
}

// LongWindowAlertsEnabled reports whether the slow-burn tier is emitted.
func (o *ObservabilitySpec) LongWindowAlertsEnabled() bool {
	return o != nil && o.PrometheusRule != nil &&
		o.PrometheusRule.LongWindowAlerts != nil && *o.PrometheusRule.LongWindowAlerts
}

// Resolved SLO accessors. The defaults are duplicated from the kubebuilder
// markers so that a hand-built spec — a unit test, a dry run — is correct too.
const (
	defaultTTFTThresholdMilli  = int64(1500)
	defaultTTFTObjectiveMilli  = int64(990)
	defaultAvailObjectiveMilli = int64(995)
)

// ResolvedTTFTThreshold returns the latency threshold in seconds.
func (s *SLOSpec) ResolvedTTFTThreshold() float64 {
	if s == nil || s.TTFTThreshold == nil {
		return float64(defaultTTFTThresholdMilli) / 1000
	}
	return float64(s.TTFTThreshold.MilliValue()) / 1000
}

// ResolvedTTFTObjective returns the latency objective as a fraction.
func (s *SLOSpec) ResolvedTTFTObjective() float64 {
	return objectiveOr(s.ttftObjective(), defaultTTFTObjectiveMilli)
}

func (s *SLOSpec) ttftObjective() *resource.Quantity {
	if s == nil {
		return nil
	}
	return s.TTFTObjective
}

// ResolvedAvailabilityObjective returns the availability objective.
func (s *SLOSpec) ResolvedAvailabilityObjective() float64 {
	if s == nil {
		return objectiveOr(nil, defaultAvailObjectiveMilli)
	}
	return objectiveOr(s.AvailabilityObjective, defaultAvailObjectiveMilli)
}

// objectiveOr resolves an objective, rejecting values outside (0, 1).
//
// An objective of exactly 1.0 would make the error budget zero and every
// burn-rate expression a division by zero — which yields +Inf and fires every
// alert permanently. Clamping to the default is the readable failure; CEL
// rejects it at admission, so this is defence in depth for an object written
// before that rule existed.
func objectiveOr(q *resource.Quantity, fallbackMilli int64) float64 {
	if q == nil {
		return float64(fallbackMilli) / 1000
	}
	v := float64(q.MilliValue()) / 1000
	if v <= 0 || v >= 1 {
		return float64(fallbackMilli) / 1000
	}
	return v
}

// ServiceMonitorSpec configures scraping of a ModelDeployment's pods.
type ServiceMonitorSpec struct {
	// Enabled creates the ServiceMonitor. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Interval is the scrape interval.
	//
	// The default is deliberately tighter than Prometheus's own 30s default.
	// Canary analysis evaluates a rate() over a short window — 60s by
	// measurement — and a rate over a window holding two samples is not a rate,
	// it is a difference between two points with no way to tell a trend from a
	// blip. 15s puts four samples in that window.
	//
	// +optional
	// +kubebuilder:default="15s"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// ScrapeTimeout bounds a single scrape. It must not exceed Interval;
	// Prometheus rejects a ServiceMonitor where it does.
	// +optional
	// +kubebuilder:default="10s"
	ScrapeTimeout *metav1.Duration `json:"scrapeTimeout,omitempty"`

	// Labels are added to the ServiceMonitor's metadata.
	//
	// This is the escape hatch for a Prometheus whose serviceMonitorSelector is
	// not empty: prometheus-operator only picks up ServiceMonitors matching
	// that selector, and the failure mode when it does not match is total
	// silence — no error, no event, simply no metrics. Setting the label the
	// local Prometheus selects on is how a user fixes that without patching the
	// operator.
	//
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// ScrapeEngineMetrics also scrapes the inference engine's own metrics
	// endpoint, in addition to the shim's. Defaults to true.
	//
	// The two are not interchangeable and neither replaces the other. The
	// shim's llmcp_* metrics are the canonical SLIs: they mean the same thing
	// under any engine, and they are what analysis and alerting consume. The
	// engine's native metrics are DIAGNOSTICS — KV-cache occupancy, slot
	// counts, tokens per second as the engine itself counts them — which are
	// invaluable when a human is debugging why a canary failed and useless as
	// something to gate a promotion on, because their names and semantics
	// change with the engine.
	//
	// +optional
	// +kubebuilder:default=true
	ScrapeEngineMetrics *bool `json:"scrapeEngineMetrics,omitempty"`
}

// ServiceMonitorEnabled reports whether a ServiceMonitor should be created.
func (o *ObservabilitySpec) ServiceMonitorEnabled() bool {
	if o == nil || o.ServiceMonitor == nil || o.ServiceMonitor.Enabled == nil {
		return true
	}
	return *o.ServiceMonitor.Enabled
}

// EngineMetricsScraped reports whether the engine's native metrics endpoint is
// scraped alongside the shim's.
func (o *ObservabilitySpec) EngineMetricsScraped() bool {
	if o == nil || o.ServiceMonitor == nil || o.ServiceMonitor.ScrapeEngineMetrics == nil {
		return true
	}
	return *o.ServiceMonitor.ScrapeEngineMetrics
}
