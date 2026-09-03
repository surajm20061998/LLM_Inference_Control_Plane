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

// Package metrics declares the names and labels of every time series this
// project emits.
//
// It exists so that the shim which PRODUCES a series and the analysis code
// which QUERIES it cannot disagree. Those two live in different binaries — the
// shim runs as a sidecar next to the engine, the analysis runs in the
// controller — and a typo in either one produces no error at all: the query
// returns an empty vector, the analysis reports "no data", and a canary either
// stalls or passes against nothing. Naming both ends from the same constants
// turns that silent failure into a compile error.
//
// # These names are API
//
// Dashboards, recording rules, alerts and user-written PromQL all reference
// them. Renaming one breaks every consumer with no deprecation path, because
// Prometheus has no notion of an alias. Add a new series rather than renaming
// an existing one.
package metrics

// Prefix is the namespace shared by every metric this project emits.
//
// By decree, `llmcp_*` is the canonical SLI that rollout analysis, autoscaling
// and SLO alerting consume. An engine's own metrics — `llamacpp_*`, `vllm:*` —
// are scraped as DIAGNOSTICS for humans reading the engine dashboard, and
// nothing automated is ever gated on them. One source of truth for decisions,
// richer data for debugging.
const Prefix = "llmcp"

// Inference metrics, emitted by the shim.
//
// The set is deliberately small. Each series here answers a question something
// downstream actually asks: the RED trio (rate, errors, duration) plus the two
// signals that are specific to token generation and cannot be derived from
// them.
const (
	// RequestsTotal counts completed inference requests.
	// Labels: model, variant, operation, code.
	//
	// The `code` label is the HTTP status as a string. It is the numerator and
	// denominator of every error-rate expression, which is why it is a label on
	// a single counter rather than two separate counters: one counter means
	// success rate and error rate are guaranteed to sum to one, whereas two
	// independently incremented counters can and do drift.
	RequestsTotal = Prefix + "_inference_requests_total"

	// RequestDurationSeconds is a histogram of end-to-end request duration.
	// Labels: model, variant, operation.
	//
	// Useful, but NOT the right primary SLI for an LLM: duration scales with
	// how many tokens the client asked for, so a p95 over mixed traffic
	// measures the request mix as much as the server. TTFT and TPOT below are
	// length-independent and are what the SLOs are written against.
	RequestDurationSeconds = Prefix + "_inference_request_duration_seconds"

	// TTFTSeconds is a histogram of time to first token.
	// Labels: model, variant.
	//
	// Recorded ONLY for streaming requests, and only from the first SSE frame
	// that carries generated text. Both restrictions are correctness
	// requirements rather than refinements — see the shim's stream reader.
	TTFTSeconds = Prefix + "_inference_ttft_seconds"

	// TPOTSeconds is a histogram of mean time per output token after the first.
	// Labels: model, variant.
	//
	// (duration - ttft) / (tokens - 1). Recorded only when at least two tokens
	// were generated, because with one token there is no inter-token interval
	// to average and dividing by zero would poison the histogram with +Inf.
	TPOTSeconds = Prefix + "_inference_tpot_seconds"

	// OutputTokensTotal counts generated tokens. Labels: model, variant.
	OutputTokensTotal = Prefix + "_inference_output_tokens_total"

	// RequestsInFlight is the number of requests currently being proxied.
	// Labels: model, variant.
	RequestsInFlight = Prefix + "_inference_requests_in_flight"

	// QueueDepth is the number of in-flight requests the engine cannot be
	// working on yet: max(0, in_flight - maxConcurrency).
	// Labels: model, variant.
	//
	// This is the autoscaling signal, and the reason it is not CPU utilisation
	// is the whole thesis of the project. An inference server saturates its
	// fixed number of decode slots long before it saturates the CPU, so CPU
	// stays unremarkable while latency climbs. Queue depth crosses its
	// threshold at the moment work actually starts waiting.
	QueueDepth = Prefix + "_inference_queue_depth"

	// UpstreamErrorsTotal counts failures reaching or reading from the engine,
	// as distinct from error responses the engine itself returned.
	// Labels: model, variant, reason.
	UpstreamErrorsTotal = Prefix + "_shim_upstream_errors_total"

	// ShimInfo is the conventional always-1 gauge carrying build and target
	// metadata as labels. Labels: model, variant, upstream, version.
	ShimInfo = Prefix + "_shim_info"
)

// Operator metrics, emitted by the controller itself onto controller-runtime's
// shared registry.
//
// These are a different KIND of series from the inference metrics above. Those
// describe the workload; these describe the control plane's decisions about it.
// The distinction matters when reading a dashboard: a rollback shows up here as
// a counter increment at the instant the controller decided, and on the
// inference metrics as whatever the latency was doing that provoked it.
//
// They exist because a dashboard panel that references a series nobody emits
// renders as "No Data", and on a rollout dashboard "No Data" is
// indistinguishable from "no rollbacks". Every series the canary dashboard
// queries is defined here and emitted below.
const (
	// CanaryPromotionsTotal counts canaries that became the primary.
	// Labels: model, namespace, name.
	CanaryPromotionsTotal = Prefix + "_canary_promotions_total"

	// CanaryRollbacksTotal counts canaries torn down.
	// Labels: model, namespace, name, reason.
	//
	// The reason label separates a rollback the ANALYSIS caused from one an
	// operator triggered and one a provider outage aborted. Collapsing them
	// would make "the gate is working" and "the monitoring broke" look
	// identical on a graph, which is the same conflation the four-valued
	// verdicts exist to prevent.
	CanaryRollbacksTotal = Prefix + "_canary_rollbacks_total"

	// CanaryCurrentWeight is the traffic share the canary is ACTUALLY
	// receiving, after quantisation. Labels: model, namespace, name.
	CanaryCurrentWeight = Prefix + "_canary_current_weight"

	// CanaryDesiredWeight is the share the current ladder step asked for.
	// Published beside CurrentWeight so the gap between them — which is
	// quantisation, not a fault — is visible rather than inferred.
	CanaryDesiredWeight = Prefix + "_canary_desired_weight"

	// CanaryFailedChecks is the current failure count against the threshold.
	CanaryFailedChecks = Prefix + "_canary_failed_checks"

	// AnalysisVerdictsTotal counts analysis rounds by outcome.
	// Labels: model, namespace, name, verdict.
	AnalysisVerdictsTotal = Prefix + "_analysis_verdicts_total"

	// AnalysisCheckDurationSeconds is how long one analysis round took.
	//
	// It bounds how quickly a rollback can be acted on: a round slower than the
	// analysis interval means the controller is permanently behind its own
	// schedule, which is invisible in every other signal.
	AnalysisCheckDurationSeconds = Prefix + "_analysis_check_duration_seconds"

	// AutoscaleDesiredReplicas is the count the built-in autoscaler last asked
	// for. Labels: model, namespace, name.
	AutoscaleDesiredReplicas = Prefix + "_autoscale_desired_replicas"
)

// Label names shared by the emitted series.
const (
	// LabelModel is spec.model.name — the served model identity, stable and
	// low-cardinality by construction.
	LabelModel = "model"

	// LabelVariant is "primary" or "canary".
	//
	// This single label is what makes canary analysis engine-independent and
	// removes the need for per-variant Services or per-variant scrape jobs: the
	// same query filtered two ways compares the two sides of a rollout.
	LabelVariant = "variant"

	// LabelOperation is the normalised request kind. See NormalizeOperation for
	// why it is normalised rather than the raw path.
	LabelOperation = "operation"

	// LabelCode is the HTTP status code as a decimal string.
	LabelCode = "code"

	// LabelReason categorises an upstream failure, or the cause of a rollback.
	LabelReason = "reason"

	// LabelNamespace and LabelName identify the ModelDeployment an operator
	// metric describes.
	//
	// Both, and not just one: `model` is a user-chosen identity that two
	// ModelDeployments can legitimately share, and `name` is unique only within
	// a namespace. A dashboard that grouped by `model` alone would silently
	// merge a staging and a production rollout of the same model into one
	// series.
	LabelNamespace = "namespace"
	LabelName      = "name"

	// LabelVerdict is an analysis round's outcome.
	LabelVerdict = "verdict"
)

// Operation label values. A closed set: an unrecognised path becomes
// OperationOther rather than a new series.
const (
	OperationChat       = "chat.completions"
	OperationCompletion = "completions"
	OperationEmbeddings = "embeddings"
	OperationModels     = "models"
	OperationOther      = "other"
)

// Upstream failure reasons.
const (
	// ReasonUnreachable means the connection to the engine failed outright.
	ReasonUnreachable = "unreachable"

	// ReasonStreamAborted means the response stream ended before its
	// terminating frame.
	ReasonStreamAborted = "stream_aborted"

	// ReasonClientCanceled means the CLIENT went away mid-stream. Counted
	// separately from a genuine failure because it is not one, and folding the
	// two together would make a load generator's own timeouts look like engine
	// errors — which, on the error-rate gate, would roll back a healthy canary.
	ReasonClientCanceled = "client_canceled"
)

// NormalizeOperation maps a request path to a bounded operation label.
//
// Prometheus label values must be a closed set. Using the raw request path
// would be fine for the OpenAI surface as it stands, but an engine that adds a
// route — or a client that appends a query string or a stray trailing segment —
// would create a new time series per distinct string. That is the classic
// cardinality explosion, and in a metrics-gated rollout it does not merely cost
// memory: the series the analysis queries stops matching the series being
// written, and the canary evaluates against no data.
func NormalizeOperation(path string) string {
	switch path {
	case "/v1/chat/completions":
		return OperationChat
	case "/v1/completions":
		return OperationCompletion
	case "/v1/embeddings":
		return OperationEmbeddings
	case "/v1/models":
		return OperationModels
	default:
		return OperationOther
	}
}
