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
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
	llmcpmetrics "github.com/surajm20061998/LLM_Inference_Control_Plane/internal/metrics"
	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// The fixture ModelDeployment these tests render a ServiceMonitor for.
const (
	testMDName    = "qwen"
	testNamespace = "demo"
)

func testInput(mutate ...func(*ServiceMonitorInput)) ServiceMonitorInput {
	in := ServiceMonitorInput{
		Name:      testMDName,
		Namespace: testNamespace,
		OwnerRef: metav1.OwnerReference{
			APIVersion:         "inference.llmcp.io/v1alpha1",
			Kind:               "ModelDeployment",
			Name:               testMDName,
			UID:                testOwnerUID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		},
		Spec:        &inferencev1alpha1.ObservabilitySpec{},
		ShimEnabled: true,
	}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

// endpoints returns the ServiceMonitor's endpoint list.
func endpoints(t *testing.T, sm *unstructured.Unstructured) []any {
	t.Helper()
	eps, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if err != nil || !found {
		t.Fatalf("spec.endpoints missing: found=%v err=%v", found, err)
	}
	return eps
}

// endpointByPort finds an endpoint by its port name.
func endpointByPort(t *testing.T, sm *unstructured.Unstructured, port string) map[string]any {
	t.Helper()
	for _, raw := range endpoints(t, sm) {
		ep, _ := raw.(map[string]any)
		if ep["port"] == port {
			return ep
		}
	}
	return nil
}

func TestServiceMonitorShape(t *testing.T) {
	sm := BuildServiceMonitor(testInput())

	if got := sm.GetAPIVersion(); got != "monitoring.coreos.com/v1" {
		t.Errorf("apiVersion = %q", got)
	}
	if got := sm.GetKind(); got != KindServiceMonitor {
		t.Errorf("kind = %q", got)
	}
	if got := sm.GetName(); got != testMDName {
		t.Errorf("name = %q, want qwen", got)
	}
	if got := sm.GetNamespace(); got != testNamespace {
		t.Errorf("namespace = %q, want demo", got)
	}

	// Ownership is what removes the ServiceMonitor when the ModelDeployment
	// goes away, and it is the reason this operator needs no finalizer.
	refs, found, _ := unstructured.NestedSlice(sm.Object, "metadata", "ownerReferences")
	if !found || len(refs) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly one", refs)
	}
	ref := refs[0].(map[string]any)
	if ref["uid"] != "uid-1" || ref["controller"] != true {
		t.Errorf("ownerReference = %v; a non-controller ref would not garbage-collect", ref)
	}
}

func TestServiceMonitorSelectsTheMetricsServiceOnly(t *testing.T) {
	// The serving Service and the metrics Service share the model-deployment
	// label. Only the component label distinguishes them, and selecting the
	// wrong one would scrape the client-facing endpoint — producing one scrape
	// per load-balancing decision rather than one per pod.
	sm := BuildServiceMonitor(testInput())

	sel, found, err := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("selector missing: %v", err)
	}
	if sel[naming.LabelComponent] != ComponentMetrics {
		t.Errorf("selector component = %q, want %q", sel[naming.LabelComponent], ComponentMetrics)
	}
	if sel[naming.LabelModelDeployment] != testMDName {
		t.Errorf("selector model-deployment = %q, want qwen", sel[naming.LabelModelDeployment])
	}

	// Restricted to one namespace: two ModelDeployments of the same name in
	// different namespaces must not merge into one job.
	names, found, _ := unstructured.NestedStringSlice(sm.Object, "spec", "namespaceSelector", "matchNames")
	if !found || len(names) != 1 || names[0] != testNamespace {
		t.Errorf("namespaceSelector.matchNames = %v, want [demo]", names)
	}
}

func TestServiceMonitorHasBothEndpoints(t *testing.T) {
	sm := BuildServiceMonitor(testInput())

	if got := len(endpoints(t, sm)); got != 2 {
		t.Fatalf("got %d endpoints, want 2 (shim SLIs and engine diagnostics)", got)
	}
	if endpointByPort(t, sm, naming.PortNameMetrics) == nil {
		t.Error("no endpoint scrapes the shim; the canonical llmcp_* SLIs would never be collected")
	}
	if endpointByPort(t, sm, naming.PortNameEngine) == nil {
		t.Error("no endpoint scrapes the engine")
	}
}

func TestEngineEndpointRewritesTheReservedColon(t *testing.T) {
	// llama.cpp names its metrics "llamacpp:prompt_tokens_total". A colon is
	// reserved by convention for recording rules, and such a series cannot be
	// written in PromQL without quoting — so every panel and ad-hoc query
	// against it fails in a way that reads like the metric is missing.
	sm := BuildServiceMonitor(testInput())

	ep := endpointByPort(t, sm, naming.PortNameEngine)
	if ep == nil {
		t.Fatal("engine endpoint missing")
	}

	relabels, _ := ep["metricRelabelings"].([]any)
	if len(relabels) != 1 {
		t.Fatalf("metricRelabelings = %v, want exactly one rule", relabels)
	}
	rule := relabels[0].(map[string]any)

	if rule["regex"] != "llamacpp:(.*)" {
		t.Errorf("regex = %v, want llamacpp:(.*)", rule["regex"])
	}
	if rule["replacement"] != "llamacpp_$1" {
		t.Errorf("replacement = %v, want llamacpp_$1", rule["replacement"])
	}
	if rule["targetLabel"] != "__name__" {
		t.Errorf("targetLabel = %v, want __name__", rule["targetLabel"])
	}
}

func TestShimDisabledOmitsTheShimEndpoint(t *testing.T) {
	// A scrape job pointing at a port no container exposes shows up in
	// Prometheus as a permanently empty job, which reads like a broken
	// exporter rather than a deliberate configuration.
	sm := BuildServiceMonitor(testInput(func(in *ServiceMonitorInput) { in.ShimEnabled = false }))

	if endpointByPort(t, sm, naming.PortNameMetrics) != nil {
		t.Error("the shim endpoint is present although the shim is disabled")
	}
	if endpointByPort(t, sm, naming.PortNameEngine) == nil {
		t.Error("the engine endpoint should remain: its diagnostics are still worth collecting")
	}
}

func TestEngineMetricsCanBeTurnedOff(t *testing.T) {
	sm := BuildServiceMonitor(testInput(func(in *ServiceMonitorInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			ServiceMonitor: &inferencev1alpha1.ServiceMonitorSpec{
				ScrapeEngineMetrics: ptr.To(false),
			},
		}
	}))

	if endpointByPort(t, sm, naming.PortNameEngine) != nil {
		t.Error("the engine endpoint is present although scrapeEngineMetrics is false")
	}
	if endpointByPort(t, sm, naming.PortNameMetrics) == nil {
		t.Error("the shim endpoint must remain; it carries the SLIs")
	}
}

func TestScrapeTimeoutIsClampedBelowInterval(t *testing.T) {
	// prometheus-operator REJECTS a ServiceMonitor whose scrapeTimeout exceeds
	// its interval, and it does so by logging and skipping the object — no
	// event, no status, no error anywhere a user will look.
	sm := BuildServiceMonitor(testInput(func(in *ServiceMonitorInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			ServiceMonitor: &inferencev1alpha1.ServiceMonitorSpec{
				Interval:      &metav1.Duration{Duration: 10 * time.Second},
				ScrapeTimeout: &metav1.Duration{Duration: 60 * time.Second},
			},
		}
	}))

	ep := endpointByPort(t, sm, naming.PortNameMetrics)
	if ep["interval"] != "10s" {
		t.Errorf("interval = %v, want 10s", ep["interval"])
	}
	if ep["scrapeTimeout"] != "10s" {
		t.Errorf("scrapeTimeout = %v, want it clamped to the interval (10s)", ep["scrapeTimeout"])
	}
}

func TestIntervalRenderedInPrometheusFormat(t *testing.T) {
	// time.Duration.String() produces "1m0s", which the prometheus-operator
	// duration validator rejects.
	sm := BuildServiceMonitor(testInput(func(in *ServiceMonitorInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			ServiceMonitor: &inferencev1alpha1.ServiceMonitorSpec{
				Interval: &metav1.Duration{Duration: 90 * time.Second},
			},
		}
	}))

	ep := endpointByPort(t, sm, naming.PortNameMetrics)
	if ep["interval"] != "90s" {
		t.Errorf("interval = %v, want 90s", ep["interval"])
	}
}

func TestUserLabelsAreMerged(t *testing.T) {
	// The escape hatch for a Prometheus whose serviceMonitorSelector is not
	// empty. When it does not match, the failure is total silence: no error, no
	// event, simply no metrics.
	sm := BuildServiceMonitor(testInput(func(in *ServiceMonitorInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			ServiceMonitor: &inferencev1alpha1.ServiceMonitorSpec{
				Labels: map[string]string{"release": "kps"},
			},
		}
	}))

	labels := sm.GetLabels()
	if labels["release"] != "kps" {
		t.Errorf("user label missing: %v", labels)
	}
	if labels[naming.LabelModelDeployment] != testMDName {
		t.Errorf("operator labels were clobbered by the merge: %v", labels)
	}
}

func TestBuildServiceMonitorIsDeterministic(t *testing.T) {
	// The result is applied with Server-Side Apply and must be byte-stable for
	// identical input, or every reconcile writes a "change", the watch fires,
	// and the controller loops against itself. Map-derived output is the usual
	// cause, and Go randomises map iteration per range statement — so a single
	// comparison would pass by luck.
	in := testInput(func(in *ServiceMonitorInput) {
		in.Spec = &inferencev1alpha1.ObservabilitySpec{
			ServiceMonitor: &inferencev1alpha1.ServiceMonitorSpec{
				Labels: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"},
			},
		}
	})

	first, err := json.Marshal(BuildServiceMonitor(in).Object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 200 {
		got, err := json.Marshal(BuildServiceMonitor(in).Object)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d produced different bytes; SSA would loop forever\nfirst: %s\ngot:   %s",
				i, first, got)
		}
	}
}

func TestDashboardsAreValidAndIdentified(t *testing.T) {
	ds, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(ds) == 0 {
		t.Fatal("no dashboards were embedded; the //go:embed pattern is not matching")
	}

	seen := map[string]bool{}
	for _, d := range ds {
		if d.UID == "" {
			t.Errorf("%s has no uid; every redeploy would create a duplicate instead of an update", d.Key)
		}
		if seen[d.UID] {
			t.Errorf("duplicate dashboard uid %q; Grafana would import one over the other", d.UID)
		}
		seen[d.UID] = true

		var parsed map[string]any
		if err := json.Unmarshal([]byte(d.JSON), &parsed); err != nil {
			t.Errorf("%s is not valid JSON: %v", d.Key, err)
		}
		if !strings.HasPrefix(d.Name, dashboardNamePrefix) {
			t.Errorf("%s has ConfigMap name %q, want the %q prefix", d.Key, d.Name, dashboardNamePrefix)
		}
	}
}

func TestDashboardsAreOrdered(t *testing.T) {
	ds, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	for i := 1; i < len(ds); i++ {
		if ds[i-1].Key >= ds[i].Key {
			t.Fatalf("dashboards are not sorted: %q before %q", ds[i-1].Key, ds[i].Key)
		}
	}
}

func TestDashboardConfigMapCarriesTheSidecarLabel(t *testing.T) {
	// The Grafana sidecar discovers dashboards by label, comparing the value
	// against a configured STRING. Writing `true` in YAML yields the label
	// value "true", which does not match — and the resulting ConfigMap exists,
	// looks correct, and is never read.
	ds, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}

	cm := DashboardConfigMap(ds[0], "llmcp-system")
	if got := cm.Labels[naming.GrafanaDashboardLabel]; got != "1" {
		t.Errorf("%s = %q, want %q", naming.GrafanaDashboardLabel, got, "1")
	}
	if got := cm.Annotations[naming.GrafanaFolderAnnotation]; got != naming.GrafanaFolder {
		t.Errorf("folder annotation = %q, want %q", got, naming.GrafanaFolder)
	}
	if _, ok := cm.Data[ds[0].Key]; !ok {
		t.Errorf("ConfigMap data key %q missing; the sidecar loads JSON values by key", ds[0].Key)
	}
}

// emittedSeries is every metric name this project produces, from any binary.
//
// It is assembled from the CONSTANTS rather than typed out, so a renamed metric
// breaks the build here instead of silently emptying a dashboard panel.
func emittedSeries() []string {
	return []string{
		// Inference metrics, from the shim.
		llmcpmetrics.RequestsStartedTotal,
		llmcpmetrics.RequestsTotal,
		llmcpmetrics.RequestDurationSeconds,
		llmcpmetrics.TTFTSeconds,
		llmcpmetrics.TPOTSeconds,
		llmcpmetrics.OutputTokensTotal,
		llmcpmetrics.OutputChunksTotal,
		llmcpmetrics.InterChunkSeconds,
		llmcpmetrics.ReportedOutputTokensTotal,
		llmcpmetrics.UsageRequestsTotal,
		llmcpmetrics.RequestsInFlight,
		llmcpmetrics.QueueDepth,
		llmcpmetrics.UpstreamErrorsTotal,
		llmcpmetrics.ShimInfo,

		// Operator metrics, from the controller.
		llmcpmetrics.CanaryPromotionsTotal,
		llmcpmetrics.CanaryRollbacksTotal,
		llmcpmetrics.CanaryCurrentWeight,
		llmcpmetrics.CanaryDesiredWeight,
		llmcpmetrics.CanaryFailedChecks,
		llmcpmetrics.AnalysisVerdictsTotal,
		llmcpmetrics.AnalysisCheckDurationSeconds,
		llmcpmetrics.AutoscaleDesiredReplicas,

		// controller-runtime's own, served on the same endpoint.
		"controller_runtime_reconcile_total",
		"controller_runtime_reconcile_errors_total",
		"controller_runtime_reconcile_time_seconds",
		"controller_runtime_active_workers",
		"workqueue_depth",

		// The engine's native metrics, after the ServiceMonitor rewrites the
		// reserved colon that llama.cpp exports them with.
		"llamacpp_requests_processing",
		"llamacpp_requests_deferred",

		// Recording rules this operator defines.
		recordTTFTSLI,
		recordAvailSLI,
		recordTTFTObjective,
		recordAvailabilityObjective,
	}
}

// seriesNamesIn extracts the metric names a PromQL expression selects.
//
// # Why not a regex over the whole expression
//
// The obvious version — match every identifier-shaped token — reports label
// VALUES (`controller="modeldeployment"`), label NAMES inside a `by (...)`
// clause, and the `e` of a scientific-notation literal like `1e-9`. Every one
// of those is a false positive, and a check that cries wolf on its own
// dashboards is a check somebody will delete.
//
// So the expression is reduced first, in the order that makes each step safe:
//
//  1. Label matchers are stripped whole. A metric name always PRECEDES its
//     `{...}`, so nothing inside a brace can be one, and removing them takes
//     every label name and value with them.
//  2. Grouping clauses are stripped, for the same reason applied to `by` and
//     `without`.
//  3. Quoted strings are stripped, catching anything the first two missed.
//  4. Only then are identifiers extracted, skipping any preceded by a digit or
//     a dot — which is what distinguishes the `e` in `1e-9` from a metric.
//
// A real PromQL parser would be exact, but it is a dependency and a maintenance
// burden for a check whose entire value is catching a typo.
func seriesNamesIn(expr string) []string {
	expr = stripBetween(expr, '{', '}')
	expr = stripGroupingClauses(expr)
	expr = stripQuoted(expr)

	var out []string
	runes := []rune(expr)

	for i := 0; i < len(runes); i++ {
		if !isIdentStart(runes[i]) {
			continue
		}
		// A digit or a dot immediately before means this is the tail of a
		// number, not a name: the `e` of `1e-9`.
		if i > 0 && (isDigit(runes[i-1]) || runes[i-1] == '.') {
			for i < len(runes) && isIdentPart(runes[i]) {
				i++
			}
			continue
		}

		start := i
		for i < len(runes) && isIdentPart(runes[i]) {
			i++
		}
		out = append(out, string(runes[start:i]))
		i--
	}
	return out
}

func isIdentStart(r rune) bool {
	return r == '_' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentPart(r rune) bool { return isIdentStart(r) || isDigit(r) }

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// stripBetween removes every balanced open..close span, including the
// delimiters.
func stripBetween(s string, open, close rune) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == open:
			depth++
		case r == close && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripGroupingClauses removes `by (...)` and `without (...)` label lists.
func stripGroupingClauses(s string) string {
	for _, kw := range []string{"by", "without", "on", "ignoring"} {
		for {
			idx := strings.Index(s, kw+" (")
			if idx < 0 {
				idx = strings.Index(s, kw+"(")
				if idx < 0 {
					break
				}
			}
			// Only treat it as a clause when the keyword stands alone.
			if idx > 0 && isIdentPart(rune(s[idx-1])) {
				break
			}
			rest := s[idx:]
			open := strings.Index(rest, "(")
			depth, end := 0, -1
			for i, r := range rest[open:] {
				if r == '(' {
					depth++
				} else if r == ')' {
					depth--
					if depth == 0 {
						end = open + i + 1
						break
					}
				}
			}
			if end < 0 {
				break
			}
			s = s[:idx] + s[idx+end:]
		}
	}
	return s
}

// stripQuoted removes double- and single-quoted spans.
func stripQuoted(s string) string {
	s = stripBetween(s, '"', '"')
	return stripBetween(s, '\'', '\'')
}

// promqlFunctions are the identifiers that are functions or operators rather
// than series names.
var promqlFunctions = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true, "count": true,
	"rate": true, "irate": true, "increase": true, "delta": true, "idelta": true,
	"histogram_quantile": true, "clamp_min": true, "clamp_max": true, "clamp": true,
	"avg_over_time": true, "sum_over_time": true, "min_over_time": true,
	"max_over_time": true, "count_over_time": true, "last_over_time": true,
	"changes": true, "resets": true, "absent": true, "vector": true, "scalar": true,
	"and": true, "or": true, "unless": true, "offset": true, "bool": true,
	"predict_linear": true, "deriv": true, "topk": true, "bottomk": true,
	"quantile": true, "stddev": true, "stdvar": true, "label_values": true,
	"time": true, "timestamp": true, "round": true, "abs": true, "ceil": true,
	"floor": true, "exp": true, "ln": true, "log2": true, "log10": true, "sqrt": true,
}

// TestEveryDashboardPanelQueriesAnEmittedSeries walks EVERY embedded dashboard.
//
// # Why this generalisation matters
//
// A panel that references a series nobody emits renders as "No Data", and on a
// health or rollout dashboard "No Data" is visually indistinguishable from "no
// problems". The failure is therefore silent in the worst possible way: the
// dashboard looks fine, and the thing it was built to show never appears.
//
// The check earned its keep immediately. The canary dashboard was written
// against operator counters — promotions, rollbacks, verdicts — that did not
// exist yet, and this test is what forced them to be implemented rather than
// shipped as three permanently empty panels.
func TestEveryDashboardPanelQueriesAnEmittedSeries(t *testing.T) {
	t.Parallel()

	known := emittedSeries()
	dashboards, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}

	checked := 0
	for _, d := range dashboards {
		for _, expr := range dashboardExprs(t, d.JSON) {
			for _, ident := range seriesNamesIn(expr) {
				if promqlFunctions[ident] || strings.HasPrefix(ident, "__") {
					continue
				}

				matched := false
				for _, series := range known {
					// Prefix match, because a histogram is queried as
					// <name>_bucket / _count / _sum and a recording rule as
					// <prefix><window>.
					if strings.HasPrefix(ident, series) {
						matched = true
						break
					}
				}
				if !matched {
					t.Errorf("dashboard %s queries %q, which nothing emits:\n  %s",
						d.Key, ident, expr)
				}
				checked++
			}
		}
	}

	if checked == 0 {
		t.Fatal("no series references were extracted at all; the check is not checking anything")
	}
}

func TestWorkloadDashboardQueriesHaveResourceScope(t *testing.T) {
	t.Parallel()
	dashboards, err := Dashboards()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dashboards {
		for _, expr := range dashboardExprs(t, d.JSON) {
			if !strings.Contains(expr, "llmcp") {
				continue
			}
			resourceScoped := strings.Contains(expr, `model_deployment="$model_deployment"`) ||
				strings.Contains(expr, `name="$model_deployment"`)
			if !strings.Contains(expr, `namespace="$namespace"`) || !resourceScoped {
				t.Errorf("unscoped workload expression in %s: %s", d.Key, expr)
			}
			if strings.Contains(expr, ":ratio_rate") && (strings.Contains(expr, "/ 0.01") || strings.Contains(expr, "/ 0.005")) {
				t.Errorf("hard-coded SLO budget in %s: %s", d.Key, expr)
			}
		}
	}
}

func TestDashboardQueriesDoNotFilterByMutableModelName(t *testing.T) {
	t.Parallel()
	dashboards, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	for _, dashboard := range dashboards {
		for _, expr := range dashboardExprs(t, dashboard.JSON) {
			if strings.Contains(expr, `model=~"$model"`) || strings.Contains(expr, `model="$model"`) {
				t.Errorf("%s filters deployment-scoped evidence by mutable model name: %s", dashboard.Key, expr)
			}
		}
	}
}

func TestEveryDashboardIsUniquelyIdentifiedAndTitled(t *testing.T) {
	t.Parallel()

	dashboards, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(dashboards) != 3 {
		t.Fatalf("got %d dashboards, want 3 (control plane, inference SLO, canary)", len(dashboards))
	}

	uids := map[string]bool{}
	for _, d := range dashboards {
		if uids[d.UID] {
			t.Errorf("duplicate uid %q; Grafana would import one over the other", d.UID)
		}
		uids[d.UID] = true
	}

	for _, want := range []string{"llmcp-control-plane", "llmcp-inference-slo", "llmcp-canary"} {
		if !uids[want] {
			t.Errorf("dashboard %s is missing", want)
		}
	}
}

func TestNoDashboardQueriesTheReservedColonForm(t *testing.T) {
	t.Parallel()

	// llama.cpp exports "llamacpp:requests_processing". A colon is reserved by
	// convention for recording rules, so the ServiceMonitor rewrites those to
	// underscores at scrape time — and a panel asking for the original form
	// silently returns nothing.
	//
	// Only EXPRESSIONS are checked: the panel descriptions deliberately quote
	// the colon form to explain why the rewrite exists.
	dashboards, err := Dashboards()
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}

	for _, d := range dashboards {
		for _, expr := range dashboardExprs(t, d.JSON) {
			if strings.Contains(expr, "llamacpp:") {
				t.Errorf("dashboard %s queries a raw llamacpp: name: %s", d.Key, expr)
			}
		}
	}
}

// dashboardExprs collects every PromQL expression in a dashboard.
func dashboardExprs(t *testing.T, body string) []string {
	t.Helper()

	var doc struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parsing dashboard: %v", err)
	}

	var out []string
	for _, p := range doc.Panels {
		for _, tgt := range p.Targets {
			if tgt.Expr != "" {
				out = append(out, tgt.Expr)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the dashboard has no panel expressions at all")
	}
	return out
}
