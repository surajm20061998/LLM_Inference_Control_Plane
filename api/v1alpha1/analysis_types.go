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
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AnalysisSpec configures the metric checks that gate a canary.
//
// # The defaults are measured, not guessed
//
// Interval 30s and Window 60s come from this project's own feasibility spike
// against a 0.6B model on a laptop CPU. The rule of thumb behind them is that a
// percentile needs on the order of 100 requests per variant to be stable; at
// the throughput measured there, 60 seconds supplies that and 30 seconds does
// not. MinRequestRate 0.5 req/s comes from the same run: below it, the p95 of
// the primary swung by more than the regression the analysis is meant to catch.
type AnalysisSpec struct {
	// Interval is how often an analysis round runs.
	// +optional
	// +kubebuilder:default="30s"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Window is the PromQL lookback each metric is evaluated over.
	//
	// It must be comfortably larger than the scrape interval, or a rate() over
	// it is computed from two samples — a difference between two points, with
	// no way to tell a trend from a blip.
	//
	// +optional
	// +kubebuilder:default="60s"
	Window *metav1.Duration `json:"window,omitempty"`

	// InitialDelay is how long to wait after the canary becomes available
	// before the first check.
	//
	// Not politeness — correctness. A freshly started inference pod has a cold
	// KV cache and, on the very first requests, is still faulting the model's
	// pages in. Its first few seconds of TTFT are genuinely terrible and
	// genuinely unrepresentative, and measuring them would fail every canary
	// that was ever going to be fine.
	//
	// +optional
	// +kubebuilder:default="60s"
	InitialDelay *metav1.Duration `json:"initialDelay,omitempty"`

	// FailureThreshold is how many FAILED checks trigger a rollback.
	//
	// The counter is NOT reset by an intervening pass, matching Flagger. A
	// canary that fails, passes, fails, passes, fails is not healthy — it is
	// intermittently broken, which for a latency SLI is the more common shape
	// of a real regression than a clean step change.
	//
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`

	// ConsecutiveErrorLimit is how many consecutive provider ERRORS abort the
	// rollout.
	//
	// Errors are counted separately from failures and are reset by any
	// successful check, because a Prometheus that is unreachable says nothing
	// whatsoever about the canary. Provider errors therefore do not spend the
	// measured-failure budget: the controller holds first, then aborts back to
	// stable only when this separate consecutive limit is reached.
	//
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	ConsecutiveErrorLimit *int32 `json:"consecutiveErrorLimit,omitempty"`

	// InconclusiveLimit is how many consecutive INCONCLUSIVE rounds trigger
	// OnInconclusive.
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	InconclusiveLimit *int32 `json:"inconclusiveLimit,omitempty"`

	// OnInconclusive is what to do once InconclusiveLimit is reached.
	// +optional
	// +kubebuilder:default=Wait
	OnInconclusive InconclusiveAction `json:"onInconclusive,omitempty"`

	// MinRequestRate is the request rate, per second and per variant, below
	// which a check is Inconclusive rather than Pass.
	//
	// This is a first-class field rather than a PromQL idiom buried in a query
	// template, because without it the entire demo is a lie: with no traffic,
	// error rate is 0 and every latency percentile is absent, so a canary
	// "passes" every check and is promoted having served nothing. Anyone who
	// has run a homegrown canary controller has shipped this bug at least once.
	//
	// Expressed as a Quantity because the Kubernetes API convention forbids
	// floats: write "500m" for half a request per second, or "2" for two.
	//
	// +optional
	// +kubebuilder:default="500m"
	MinRequestRate *resource.Quantity `json:"minRequestRate,omitempty"`

	// MinUsageSamples is the minimum number of successful responses with
	// explicit completion-token usage in each variant's measurement window
	// before output-token-rate can be evaluated. Missing usage is not inferred.
	// +optional
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	MinUsageSamples *int32 `json:"minUsageSamples,omitempty"`

	// Provider points at the metric backend.
	// +optional
	Provider AnalysisProviderSpec `json:"provider,omitempty"`

	// Metrics are the checks evaluated each round. When empty, a sensible
	// default set is used — see DefaultMetrics.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=10
	Metrics []AnalysisMetric `json:"metrics,omitempty"`
}

// InconclusiveAction is what to do when a canary cannot be judged.
// +kubebuilder:validation:Enum=Wait;Rollback;Promote
type InconclusiveAction string

const (
	// InconclusiveWait holds the canary at its current weight indefinitely.
	//
	// The default, and the only safe one to default to. "We cannot tell" is not
	// evidence of health and is not evidence of harm; holding position keeps
	// the blast radius fixed and leaves the decision to a human, which is what
	// should happen when the automation has run out of information.
	InconclusiveWait InconclusiveAction = "Wait"

	// InconclusiveRollback reverts. Appropriate when no traffic reaching the
	// canary is itself a symptom worth acting on.
	InconclusiveRollback InconclusiveAction = "Rollback"

	// InconclusivePromote continues as though the checks had passed.
	//
	// Deliberately available and deliberately not the default: it is correct
	// for a genuinely low-traffic service where waiting forever is the worse
	// outcome, and dangerous everywhere else.
	InconclusivePromote InconclusiveAction = "Promote"
)

// AnalysisProviderType selects the metric backend.
// +kubebuilder:validation:Enum=Prometheus
type AnalysisProviderType string

// AnalysisProviderPrometheus queries a Prometheus HTTP API.
const AnalysisProviderPrometheus AnalysisProviderType = "Prometheus"

// AnalysisProviderSpec points at the metric backend.
type AnalysisProviderSpec struct {
	// Type selects the backend implementation.
	// +optional
	// +kubebuilder:default=Prometheus
	Type AnalysisProviderType `json:"type,omitempty"`

	// Address is the base URL of the metric backend, e.g.
	// http://prometheus-operated.monitoring.svc:9090
	//
	// It has no default. A wrong-but-plausible default would make every canary
	// report Error for a reason nobody would think to check, and unlike most
	// misconfigurations this one is silent until a rollout is already in
	// flight.
	//
	// +optional
	Address string `json:"address,omitempty"`

	// Timeout bounds a single query.
	// +optional
	// +kubebuilder:default="10s"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// BuiltinMetric names a query this operator writes, correctly, once.
//
// Raw PromQL in user hands is a NaN factory: an unclamped denominator, a
// forgotten rate(), a window shorter than the scrape interval, and the result
// is a number that looks fine and means nothing. Each built-in below is written
// with the denominator clamped away from zero and the window supplied from
// AnalysisSpec, so the common cases cannot be got wrong. The Query escape hatch
// remains for the cases that are not common.
//
// +kubebuilder:validation:Enum=ttft-p95;ttft-p99;request-duration-p95;error-rate;success-rate;queue-depth;output-token-rate;output-chunk-rate
type BuiltinMetric string

const (
	// MetricTTFTP95 is the 95th percentile time to first token, in seconds.
	// The primary latency SLI: length-independent, unlike total duration.
	MetricTTFTP95 BuiltinMetric = "ttft-p95"

	// MetricTTFTP99 is the 99th percentile time to first token.
	MetricTTFTP99 BuiltinMetric = "ttft-p99"

	// MetricRequestDurationP95 is the 95th percentile end-to-end duration.
	// Useful, but it scales with output length, so a change can mean the
	// request mix moved rather than the server.
	MetricRequestDurationP95 BuiltinMetric = "request-duration-p95"

	// MetricErrorRate is the share of requests answered 5xx, as a fraction
	// in [0,1].
	MetricErrorRate BuiltinMetric = "error-rate"

	// MetricSuccessRate is 1 - error rate.
	MetricSuccessRate BuiltinMetric = "success-rate"

	// MetricQueueDepth is the mean number of requests waiting for a slot.
	MetricQueueDepth BuiltinMetric = "queue-depth"

	// MetricOutputTokenRate is explicit completion-token usage per second from
	// successful completed responses. MinUsageSamples gates the measurement;
	// missing usage is never estimated from stream chunks.
	MetricOutputTokenRate BuiltinMetric = "output-token-rate"

	// MetricOutputChunkRate counts observable content-bearing SSE events per
	// second. A chunk can contain multiple tokenizer tokens.
	MetricOutputChunkRate BuiltinMetric = "output-chunk-rate"
)

// AnalysisMetric is one gate a canary must pass.
//
// +kubebuilder:validation:XValidation:rule="has(self.builtin) != has(self.query)",message="set exactly one of builtin and query"
// +kubebuilder:validation:XValidation:rule="has(self.thresholdRange.min) || has(self.thresholdRange.max)",message="thresholdRange must set at least one of min and max"
type AnalysisMetric struct {
	// Name identifies the check in status and events. It is the list map key,
	// so it must be unique within the spec.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Builtin selects a query this operator writes.
	// +optional
	Builtin *BuiltinMetric `json:"builtin,omitempty"`

	// Query is raw PromQL, for checks the built-ins do not cover.
	//
	// Identity variables {{.Namespace}}, {{.ModelDeployment}}, {{.Model}} and
	// {{.Variant}} are substituted as escaped contents for double-quoted label
	// matchers; {{.Window}} supplies the analysis lookback. Custom queries are
	// not isolated automatically and must select the resource identity.
	// A query returning more than one series is an Error, not a silent
	// first-match — picking arbitrarily from an ambiguous result is how a gate
	// ends up measuring the wrong pod.
	//
	// +optional
	Query string `json:"query,omitempty"`

	// ThresholdRange is the band the value must fall inside to Pass.
	// +required
	ThresholdRange ThresholdRange `json:"thresholdRange"`

	// CompareToPrimary evaluates the RATIO of the canary's value to the
	// primary's rather than the canary's absolute value.
	//
	// Usually the better gate for latency. An absolute TTFT threshold has to be
	// re-tuned for every model, every node type and every prompt length, and
	// one that is stale fires on a busy afternoon rather than on a bad release.
	// A ratio asks the only question that matters — is the new version worse
	// than the one it is replacing? — and answers it under whatever conditions
	// happen to be true right now.
	//
	// +optional
	// +kubebuilder:default=false
	CompareToPrimary *bool `json:"compareToPrimary,omitempty"`
}

// ThresholdRange is an inclusive band. At least one bound must be set.
//
// Both bounds are resource.Quantity, not float64, because the Kubernetes API
// convention forbids floats in a spec — they serialise inconsistently across
// language bindings and cannot round-trip through the API server's protobuf
// encoding. Write max: "1.5" or max: "1500m".
type ThresholdRange struct {
	// Min is the inclusive lower bound.
	// +optional
	Min *resource.Quantity `json:"min,omitempty"`

	// Max is the inclusive upper bound.
	// +optional
	Max *resource.Quantity `json:"max,omitempty"`
}

// Verdict is the outcome of one metric check.
//
// Four values, with DISJOINT counters, because collapsing them into pass/fail
// is how canary controllers act on the wrong evidence. The distinction that
// matters most: a Prometheus outage yields Error, never Fail, and does not
// spend the failed-check budget. The state machine holds initially and aborts
// safely to the stable revision at the configured consecutive-error limit.
//
// +kubebuilder:validation:Enum=Pass;Fail;Inconclusive;Error
type Verdict string

const (
	// VerdictPass means the query succeeded and the value is inside the
	// threshold range. It resets the Error and Inconclusive counters but NOT
	// the failure counter — see AnalysisSpec.FailureThreshold.
	VerdictPass Verdict = "Pass"

	// VerdictFail means the query succeeded and the value is outside the
	// range. This is the only verdict that counts toward a rollback.
	VerdictFail Verdict = "Fail"

	// VerdictInconclusive means the query succeeded but the answer is
	// unusable: no samples, NaN, or a request rate below MinRequestRate.
	VerdictInconclusive Verdict = "Inconclusive"

	// VerdictError means the provider could not be queried, or answered with
	// something that is not a number.
	VerdictError Verdict = "Error"
)

// MetricCheck records one metric's outcome for status.
type MetricCheck struct {
	// Name is the AnalysisMetric this check evaluated.
	// +required
	Name string `json:"name"`

	// Verdict is the outcome.
	// +required
	Verdict Verdict `json:"verdict"`

	// Value is the observed value, rendered as a string.
	//
	// A string rather than a Quantity: this is an observation, not a
	// specification, so it is not bound by the no-floats convention, and it
	// must be able to say "NaN" or stay empty when there was no value at all.
	// A zero here would be indistinguishable from a measured zero.
	//
	// +optional
	Value string `json:"value,omitempty"`

	// Threshold restates the band that was applied, so a status read weeks
	// later still explains itself without the spec beside it.
	// +optional
	Threshold string `json:"threshold,omitempty"`

	// Message explains a non-Pass verdict.
	// +optional
	Message string `json:"message,omitempty"`
}

// CanaryStatus reports the state of an in-flight or just-finished canary.
type CanaryStatus struct {
	// Revision is the revision hash under evaluation.
	// +optional
	Revision string `json:"revision,omitempty"`

	// StableRevision is what a rollback would revert to.
	// +optional
	StableRevision string `json:"stableRevision,omitempty"`

	// FailedRevision is the revision the most recent rollback rejected.
	//
	// Without it the controller would immediately re-canary the same revision
	// it just rolled back from: the spec still names it, so the target and the
	// stable revision still differ, and the state machine would dutifully start
	// again — producing an infinite loop of identical failing rollouts, each
	// one costing a full analysis window and a Deployment churn.
	//
	// Recording the rejection makes a rollback STICK until a human changes the
	// spec, which is exactly the semantics people expect from one.
	//
	// +optional
	FailedRevision string `json:"failedRevision,omitempty"`

	// DesiredWeight is the traffic share the current step asked for.
	// +optional
	DesiredWeight int32 `json:"desiredWeight,omitempty"`

	// CurrentWeight is the configured replica share after quantisation.
	//
	// Reported separately from DesiredWeight because replica-based splitting
	// cannot configure 20% with 3 pods: the nearest share is 33%. This field is
	// derived from desired replica counts, not measured request distribution;
	// connection reuse can make observed traffic differ from the pod ratio.
	//
	// +optional
	CurrentWeight int32 `json:"currentWeight,omitempty"`

	// ObservedWeight is the rounded percentage of inference requests started at
	// canary shims over ObservationWindow. It is absent when evidence is missing,
	// stale or below the traffic floor. It never drives rollout decisions.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	ObservedWeight *int32 `json:"observedWeight,omitempty"`

	// ObservedAt is the shared Prometheus evaluation timestamp of the last
	// observation attempt, including attempts that yielded an unknown weight.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// ObservationWindow is the lookback used for the request-start rates.
	// +optional
	ObservationWindow *metav1.Duration `json:"observationWindow,omitempty"`

	// ObservationReason explains whether the measurement is available. An
	// unknown observation does not spend any rollout failure or error budget.
	// +optional
	ObservationReason string `json:"observationReason,omitempty"`

	// MetricScopeReady reports whether the controller has observed a complete
	// analysis window from shims carrying this ModelDeployment's namespace and
	// name labels. Automatic analysis holds while this is false so metrics from
	// another resource, or from a pinned legacy shim, cannot drive the rollout.
	// +optional
	MetricScopeReady bool `json:"metricScopeReady,omitempty"`

	// Step is the zero-based index into the weight ladder.
	// +optional
	Step int32 `json:"step,omitempty"`

	// Steps is how many steps the ladder has.
	// +optional
	Steps int32 `json:"steps,omitempty"`

	// FailedChecks counts rounds that produced a Fail. Not reset by a
	// subsequent Pass.
	// +optional
	FailedChecks int32 `json:"failedChecks,omitempty"`

	// ConsecutiveErrors counts back-to-back provider failures. Reset by any
	// round that reaches a verdict.
	// +optional
	ConsecutiveErrors int32 `json:"consecutiveErrors,omitempty"`

	// ConsecutiveInconclusive counts back-to-back unusable rounds.
	// +optional
	ConsecutiveInconclusive int32 `json:"consecutiveInconclusive,omitempty"`

	// Checks is the most recent round's per-metric outcomes.
	// +optional
	// +listType=atomic
	Checks []MetricCheck `json:"checks,omitempty"`

	// StartTime is when this canary began.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// LastAnalysisTime is when the last round ran. It is what the interval is
	// measured from.
	// +optional
	LastAnalysisTime *metav1.Time `json:"lastAnalysisTime,omitempty"`

	// AvailableSince is when the canary's pods first became ready.
	//
	// The warm-up delay is measured from here rather than from StartTime,
	// because a canary that spent four minutes pulling a model image has not
	// been warm for four minutes — and measuring a cold inference pod's first
	// requests would fail every canary that was ever going to be fine.
	//
	// +optional
	AvailableSince *metav1.Time `json:"availableSince,omitempty"`

	// ReadinessTarget binds AvailableSince to the current rung, capacity and
	// candidate Deployment. A change requires fresh warm-up and evidence.
	// +optional
	ReadinessTarget CanaryReadinessTarget `json:"readinessTarget,omitempty"`

	// ReadyReplicas is the observed ready candidate count, distinct from the
	// desired count in ReadinessTarget. Old-generation pods do not satisfy the gate.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the observed available candidate count.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// Message is a human-readable summary of the current state.
	// +optional
	Message string `json:"message,omitempty"`
}

// Resolved accessors for AnalysisSpec, so no caller repeats a default.

// The defaults below are duplicated from the kubebuilder markers on purpose.
// The API server fills them in for any object that went through admission, but
// these functions must also be correct for a hand-built spec — a unit test, or
// a future dry-run path — where nothing has been defaulted at all.
const (
	defaultAnalysisInterval      = 30 * time.Second
	defaultAnalysisWindow        = 60 * time.Second
	defaultAnalysisInitialDelay  = 60 * time.Second
	defaultQueryTimeout          = 10 * time.Second
	defaultFailureThreshold      = int32(3)
	defaultConsecutiveErrorLimit = int32(5)
	defaultInconclusiveLimit     = int32(5)
	defaultMinRequestRateMilli   = int64(500)
)

// ResolvedInterval returns the analysis interval.
func (a *AnalysisSpec) ResolvedInterval() metav1.Duration {
	return durationOr(a.getInterval(), defaultAnalysisInterval)
}

func (a *AnalysisSpec) getInterval() *metav1.Duration {
	if a == nil {
		return nil
	}
	return a.Interval
}

// ResolvedWindow returns the PromQL lookback window.
func (a *AnalysisSpec) ResolvedWindow() metav1.Duration {
	if a == nil {
		return metav1.Duration{Duration: defaultAnalysisWindow}
	}
	return durationOr(a.Window, defaultAnalysisWindow)
}

// ResolvedInitialDelay returns the warm-up grace before the first check.
func (a *AnalysisSpec) ResolvedInitialDelay() metav1.Duration {
	if a == nil {
		return metav1.Duration{Duration: defaultAnalysisInitialDelay}
	}
	return durationOr(a.InitialDelay, defaultAnalysisInitialDelay)
}

// ResolvedTimeout returns the per-query timeout.
func (a *AnalysisSpec) ResolvedTimeout() metav1.Duration {
	if a == nil {
		return metav1.Duration{Duration: defaultQueryTimeout}
	}
	return durationOr(a.Provider.Timeout, defaultQueryTimeout)
}

// ResolvedFailureThreshold returns how many failures trigger a rollback.
func (a *AnalysisSpec) ResolvedFailureThreshold() int32 {
	if a == nil {
		return defaultFailureThreshold
	}
	return int32Or(a.FailureThreshold, defaultFailureThreshold)
}

// ResolvedConsecutiveErrorLimit returns the provider-error tolerance.
func (a *AnalysisSpec) ResolvedConsecutiveErrorLimit() int32 {
	if a == nil {
		return defaultConsecutiveErrorLimit
	}
	return int32Or(a.ConsecutiveErrorLimit, defaultConsecutiveErrorLimit)
}

// ResolvedInconclusiveLimit returns the unusable-round tolerance.
func (a *AnalysisSpec) ResolvedInconclusiveLimit() int32 {
	if a == nil {
		return defaultInconclusiveLimit
	}
	return int32Or(a.InconclusiveLimit, defaultInconclusiveLimit)
}

// ResolvedOnInconclusive returns what to do once the limit is reached.
func (a *AnalysisSpec) ResolvedOnInconclusive() InconclusiveAction {
	if a == nil || a.OnInconclusive == "" {
		return InconclusiveWait
	}
	return a.OnInconclusive
}

// ResolvedMinRequestRate returns the traffic floor, in requests per second.
//
// Returned as a float because it is compared against a query result, which is a
// float. The Quantity type exists to keep the SPEC free of floats; converting
// at the boundary is the intended use.
func (a *AnalysisSpec) ResolvedMinRequestRate() float64 {
	if a == nil || a.MinRequestRate == nil {
		return float64(defaultMinRequestRateMilli) / 1000
	}
	return float64(a.MinRequestRate.MilliValue()) / 1000
}

// ResolvedMinUsageSamples returns the usage-bearing completion sample floor.
func (a *AnalysisSpec) ResolvedMinUsageSamples() int32 {
	if a == nil || a.MinUsageSamples == nil || *a.MinUsageSamples < 1 {
		return 20
	}
	return *a.MinUsageSamples
}

// CompareToPrimaryEnabled reports whether this metric is evaluated as a ratio
// against the primary rather than as an absolute value.
func (m *AnalysisMetric) CompareToPrimaryEnabled() bool {
	return m != nil && m.CompareToPrimary != nil && *m.CompareToPrimary
}

func durationOr(d *metav1.Duration, fallback time.Duration) metav1.Duration {
	if d == nil || d.Duration <= 0 {
		return metav1.Duration{Duration: fallback}
	}
	return *d
}

func int32Or(v *int32, fallback int32) int32 {
	if v == nil || *v < 1 {
		return fallback
	}
	return *v
}

// DefaultMetrics is the check set used when spec omits metrics entirely.
//
// # Why there is a default at all
//
// A canary spec with no metrics is not a canary: it is a timed rollout that
// waits and then promotes regardless of what happened. Requiring the user to
// write the checks would be defensible, but the failure mode of forgetting is
// silent success — the rollout promotes, everything looks fine, and nobody
// learns that the gate was never armed. Defaulting to a real gate means the
// worst case of leaving the field blank is an over-cautious rollout rather than
// an unguarded one.
//
// # Why these two
//
// TTFT as a RATIO against the primary, and error rate as an absolute.
//
// The ratio is the important choice. An absolute latency threshold has to be
// re-tuned per model, per node type and per prompt mix, and a stale one fires
// on a busy afternoon rather than on a bad release. A ratio asks the only
// question a rollout actually needs answered — is the new version worse than
// the one it is replacing? — and answers it under whatever conditions happen to
// hold at the time. 1.5 tolerates ordinary variance while catching the kind of
// regression the feasibility spike measured, where a starved canary ran better
// than 2x its primary.
//
// Error rate stays absolute, because a ratio is meaningless when the primary's
// error rate is zero — which it should be, and which would make every ratio
// either 0 or undefined.
func DefaultMetrics() []AnalysisMetric {
	ttft := MetricTTFTP95
	errRate := MetricErrorRate
	yes := true

	return []AnalysisMetric{
		{
			Name:             "ttft-vs-primary",
			Builtin:          &ttft,
			CompareToPrimary: &yes,
			ThresholdRange:   ThresholdRange{Max: quantity("1500m")},
		},
		{
			Name:           "error-rate",
			Builtin:        &errRate,
			ThresholdRange: ThresholdRange{Max: quantity("50m")},
		},
	}
}

// ResolvedMetrics returns the checks to run, falling back to DefaultMetrics.
func (a *AnalysisSpec) ResolvedMetrics() []AnalysisMetric {
	if a == nil || len(a.Metrics) == 0 {
		return DefaultMetrics()
	}
	return a.Metrics
}

// quantity parses a canonical quantity literal, panicking on a malformed one.
//
// The panic is safe and deliberate: every call site passes a compile-time
// constant, so a failure here can only be a typo introduced while editing this
// file, and it would be caught by the first test that runs. The alternative —
// returning an error nobody can handle — would push a nil Quantity into a
// threshold comparison, where it silently means "no bound".
func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
