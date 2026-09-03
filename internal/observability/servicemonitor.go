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
	"maps"
	"slices"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// Default scrape timings, used when spec.observability leaves them unset.
const (
	// defaultInterval is tighter than Prometheus's own 30s default because
	// analysis evaluates a rate() over a 60s window and a two-sample rate is
	// not a rate.
	defaultInterval = 15 * time.Second

	// defaultScrapeTimeout must stay below defaultInterval; prometheus-operator
	// rejects a ServiceMonitor where it does not.
	defaultScrapeTimeout = 10 * time.Second

	// engineScrapeInterval is looser than the shim's. Engine metrics are
	// diagnostics that a human reads after the fact, never something a
	// promotion is gated on, so paying for them at the same cadence as the
	// SLIs would be spending scrape budget on the wrong thing.
	engineScrapeInterval = 30 * time.Second
)

// ServiceMonitorInput is everything needed to render a ServiceMonitor.
type ServiceMonitorInput struct {
	// Name and Namespace of the owning ModelDeployment.
	Name      string
	Namespace string

	// OwnerRef makes the ServiceMonitor a child of the ModelDeployment, so
	// Kubernetes garbage collection removes it. That is why there is no
	// finalizer and no explicit cleanup path.
	OwnerRef metav1.OwnerReference

	// Spec is the user's observability configuration; a nil-safe zero value is
	// acceptable.
	Spec *inferencev1alpha1.ObservabilitySpec

	// ShimEnabled says whether the metrics sidecar is injected. When it is not,
	// the shim endpoint is omitted entirely rather than left pointing at a port
	// no container exposes — prometheus-operator would otherwise generate a
	// scrape job with no targets, which shows up in Prometheus as a permanently
	// empty job and reads like a broken exporter.
	ShimEnabled bool
}

// BuildServiceMonitor renders the ServiceMonitor for one ModelDeployment.
//
// # Two endpoints, on purpose
//
// The first scrapes the shim and yields the llmcp_* series that rollout
// analysis, autoscaling and SLO alerting read. The second scrapes the engine's
// own /metrics for diagnostics. Keeping them as separate endpoints on one
// ServiceMonitor — rather than one endpoint, or two ServiceMonitors — means the
// two can carry different intervals and different relabelling while remaining a
// single object whose lifecycle follows the ModelDeployment's.
//
// # Determinism
//
// The result is applied with Server-Side Apply and must be byte-stable for
// identical input, or every reconcile writes a "change", the watch fires, and
// the controller loops. Nested maps here are fine — unstructured content is
// serialised through encoding/json, which sorts map keys — but any SLICE built
// from a map must be sorted explicitly. Only Labels is map-derived, and it goes
// into a map.
func BuildServiceMonitor(in ServiceMonitorInput) *unstructured.Unstructured {
	labels := naming.CommonLabels(in.Name)
	if in.Spec != nil && in.Spec.ServiceMonitor != nil {
		maps.Copy(labels, in.Spec.ServiceMonitor.Labels)
	}

	interval, timeout := scrapeTimings(in.Spec)

	endpoints := make([]any, 0, 2)
	if in.ShimEnabled {
		endpoints = append(endpoints, shimEndpoint(interval, timeout))
	}
	if in.Spec.EngineMetricsScraped() {
		endpoints = append(endpoints, engineEndpoint(timeout))
	}

	sm := &unstructured.Unstructured{Object: map[string]any{
		fieldAPIVersion: MonitoringGroup + "/" + MonitoringVersion,
		fieldKind:       KindServiceMonitor,
		fieldMetadata: map[string]any{
			fieldName:            naming.ServiceMonitor(in.Name),
			fieldNamespace:       in.Namespace,
			fieldLabels:          toStringMap(labels),
			fieldOwnerReferences: []any{ownerRefMap(in.OwnerRef)},
		},
		fieldSpec: map[string]any{
			// jobLabel names the `job` label on every scraped series after the
			// Service's own label of that name. Without it every series from
			// every ModelDeployment shares job="<namespace>/<service>", which
			// is legible but not groupable.
			"jobLabel": naming.LabelInstance,

			// Selects the HEADLESS metrics Service, not the serving one. The
			// component label is what distinguishes them, and it is the reason
			// scraping cannot accidentally target the client-facing endpoint —
			// which would produce one scrape per request-routing decision
			// rather than one per pod.
			"selector": map[string]any{
				"matchLabels": map[string]any{
					naming.LabelModelDeployment: in.Name,
					naming.LabelComponent:       ComponentMetrics,
				},
			},

			// Restricted to the ModelDeployment's own namespace. The default is
			// to search everywhere, which in a cluster running two
			// ModelDeployments of the same name in different namespaces would
			// silently scrape both into one job.
			"namespaceSelector": map[string]any{
				"matchNames": []any{in.Namespace},
			},

			// Copy the variant and revision pod labels onto every scraped
			// series. The shim already emits `variant` itself, so this is for
			// the ENGINE endpoint, whose native metrics have no idea which side
			// of a rollout they came from — without this the engine dashboard
			// cannot separate a canary from its primary at all.
			"podTargetLabels": []any{
				naming.LabelVariant,
				naming.LabelRevision,
			},

			"endpoints": endpoints,
		},
	}}

	return sm
}

// labelMetricName is Prometheus's reserved label holding a series' name. The
// engine relabelling rule reads it and writes it back, which is how a metric is
// renamed at scrape time.
const labelMetricName = "__name__"

// ComponentMetrics is the app.kubernetes.io/component value on the headless
// metrics Service. It is what the ServiceMonitor's selector keys off, so it is
// declared here next to the selector that reads it.
const ComponentMetrics = "metrics"

// shimEndpoint scrapes the canonical llmcp_* SLIs.
func shimEndpoint(interval, timeout time.Duration) map[string]any {
	return map[string]any{
		fieldPort:          naming.PortNameMetrics,
		fieldPath:          naming.ShimMetricsPath,
		fieldScheme:        "http",
		fieldInterval:      duration(interval),
		fieldScrapeTimeout: duration(timeout),
		// honorLabels: false. If the shim ever emitted a label that collides
		// with one Prometheus attaches (job, instance, namespace), the target's
		// value must win — otherwise a compromised or buggy exporter could
		// relabel its own series into another workload's identity, which for a
		// promotion gate reading those series is not a theoretical problem.
		fieldHonorLabels: false,
	}
}

// engineEndpoint scrapes the engine's native metrics as diagnostics.
func engineEndpoint(timeout time.Duration) map[string]any {
	return map[string]any{
		fieldPort:          naming.PortNameEngine,
		fieldPath:          naming.ShimMetricsPath,
		fieldScheme:        "http",
		fieldInterval:      duration(engineScrapeInterval),
		fieldScrapeTimeout: duration(timeout),
		fieldHonorLabels:   false,
		"metricRelabelings": []any{
			// llama.cpp names its metrics `llamacpp:prompt_tokens_total` and
			// friends. A colon is legal in the data model but is RESERVED by
			// convention for recording rules, and the practical consequence is
			// that such a series cannot be written in PromQL without quoting —
			// so every dashboard panel and every ad-hoc query against it fails
			// in a way that reads like the metric is missing. Upstream tracks
			// this as ggml-org/llama.cpp#12803; rewriting at scrape time is the
			// fix available to a consumer.
			map[string]any{
				"sourceLabels": []any{labelMetricName},
				"regex":        "llamacpp:(.*)",
				"targetLabel":  labelMetricName,
				"replacement":  "llamacpp_$1",
				"action":       "replace",
			},
		},
	}
}

// scrapeTimings resolves the scrape interval and timeout.
//
// The timeout is clamped below the interval rather than passed through.
// prometheus-operator REJECTS a ServiceMonitor whose scrapeTimeout exceeds its
// interval, and it rejects it by logging and skipping the object — no event, no
// status, no error anywhere the user will look. A configuration that would be
// rejected is therefore corrected here, where the correction is at least
// visible in the applied object.
func scrapeTimings(spec *inferencev1alpha1.ObservabilitySpec) (interval, timeout time.Duration) {
	interval, timeout = defaultInterval, defaultScrapeTimeout

	if spec != nil && spec.ServiceMonitor != nil {
		if v := spec.ServiceMonitor.Interval; v != nil && v.Duration > 0 {
			interval = v.Duration
		}
		if v := spec.ServiceMonitor.ScrapeTimeout; v != nil && v.Duration > 0 {
			timeout = v.Duration
		}
	}

	if timeout > interval {
		timeout = interval
	}
	return interval, timeout
}

// duration renders a Go duration in the Prometheus duration format.
//
// time.Duration.String() produces "1m0s" and "1.5s", neither of which the
// prometheus-operator duration validator accepts. Emitting whole seconds is
// unambiguous, round-trips, and matches how every scrape interval is written by
// hand anyway.
func duration(d time.Duration) string {
	secs := max(int64(d.Round(time.Second)/time.Second), 1)
	return strconv.FormatInt(secs, 10) + "s"
}

// toStringMap converts a label map into the any-valued form unstructured
// content requires.
//
// The keys are sorted before insertion. That is not needed for correctness —
// a map has no order — but it keeps this function's behaviour obvious to the
// next reader, who will reasonably be scanning for the map-iteration bug that
// breaks Server-Side Apply idempotency elsewhere in this codebase.
func toStringMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for _, k := range slices.Sorted(maps.Keys(in)) {
		out[k] = in[k]
	}
	return out
}

// ownerRefMap renders an OwnerReference as unstructured content.
func ownerRefMap(ref metav1.OwnerReference) map[string]any {
	m := map[string]any{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"name":       ref.Name,
		"uid":        string(ref.UID),
	}
	if ref.Controller != nil {
		m["controller"] = *ref.Controller
	}
	if ref.BlockOwnerDeletion != nil {
		m["blockOwnerDeletion"] = *ref.BlockOwnerDeletion
	}
	return m
}
