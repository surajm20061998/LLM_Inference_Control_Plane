# Metrics and decision evidence

The shim emits canonical `llmcp_*` workload metrics. Every canonical shim series carries `namespace`, `model_deployment`, `model`, and `variant`. The first two identify the owning Kubernetes resource; `model` describes the model serving that response, and `variant` is `primary` or `canary`. Prompts, responses, user identifiers and request identifiers are never metric labels.

Built-in canary queries select namespace, ModelDeployment name and variant. Autoscaling queries select namespace and ModelDeployment name and aggregate both variants. These selectors deliberately omit `model`: during a model update the primary still serves the previous model, so selecting the candidate's model name would hide stable evidence and some fleet demand. Resource recreation and rollout transitions still require fresh measurement windows; resource-name labels alone do not encode revision identity.

## Running a standalone shim

The controller supplies identity flags automatically. A manually started shim must supply them as well:

```sh
go run ./cmd/shim --namespace=team-a --model-deployment=chat \
  --model=qwen3 --variant=primary --upstream=http://127.0.0.1:8000 \
  --max-concurrency=4
```

The default serving port is 8080 and the separate metrics listener is 9090. `--max-concurrency` must agree with engine capacity because queue depth is estimated as `max(in_flight - max_concurrency, 0)`. Missing namespace or resource-name flags cause startup to fail. `--help` lists all listener, health, logging and shutdown flags.

## What each measurement means

| Metric | Meaning |
| --- | --- |
| `llmcp_inference_requests_started_total` | Admission of POST requests to `/v1/chat/completions`, `/v1/completions` and `/v1/embeddings`, labeled by operation, before the upstream responds. Failed and canceled admitted requests still count. Health probes, model discovery, unknown routes and non-POST calls do not count. |
| `llmcp_inference_requests_total` | Completed proxy requests by normalized operation and HTTP status. An aborted stream still records its response status; separate upstream-error metrics identify read failures. |
| `llmcp_inference_request_duration_seconds` | End-to-end proxy request duration. This depends on requested output length. |
| `llmcp_inference_ttft_seconds` | Time until the first content-bearing SSE event is read from upstream. Role-only events and keep-alives do not count. This is observable first-content latency, not engine-internal tokenizer timing. |
| `llmcp_inference_output_chunks_total` | First-choice SSE events containing text, reasoning content, or tool-call function names/arguments. One event can contain several tokenizer tokens. |
| `llmcp_inference_inter_chunk_seconds` | Observed interval between successive content-bearing SSE events. Events arriving in the same network read can have near-zero intervals. |
| `llmcp_inference_reported_output_tokens_total` | Explicit nonnegative integer `usage.completion_tokens` totals from successful completed responses. Missing usage is never estimated. |
| `llmcp_inference_usage_requests_total` | Number of successful completed responses that supplied valid completion-token usage, including an explicit zero. |
| `llmcp_inference_requests_in_flight` | Requests currently being proxied. |
| `llmcp_inference_queue_depth` | Estimated requests beyond configured engine concurrency, rather than an engine scheduler's queue measurement. |

Streaming usage is counted only after `[DONE]` with no upstream read error and a successful HTTP status. A final usage event is not itself a content chunk. For `application/json` responses the observer inspects at most 1 MiB and only accepts a complete valid JSON body. Oversized or malformed observations are ignored for usage accounting while response bytes are forwarded unchanged. SSE event buffering is also bounded to 1 MiB. The shim does not rewrite requests to force usage reporting: clients must request it when their server's protocol requires that option. Reported totals remain server-provided evidence, not independently verified billing data.

## Token-throughput evidence gate

The `output-token-rate` built-in reads the reported-token counter. Before evaluating it, analysis requires at least `spec.rollout.canary.analysis.minUsageSamples` usage-bearing successful completions in the configured lookback window; the default is **20**. Relative checks require the floor independently in the primary and canary. Absolute checks require it only in the canary. Missing, nonfinite or insufficient evidence is `Inconclusive`; a provider failure is `Error`. The floor uses Prometheus `increase`, which can be fractional because of scrape extrapolation, and does not round a shortfall upward.

`output-chunk-rate` reads the content-chunk counter and needs no reported-usage floor. Both checks still obey the configured general request-rate gate. Chunk throughput depends on server chunking and is not interchangeable with token throughput.

## Custom PromQL

Custom queries are not automatically isolated. Use exact-match resource identity selectors, for example:

```promql
sum(rate(llmcp_inference_reported_output_tokens_total{namespace="{{.Namespace}}",model_deployment="{{.ModelDeployment}}",variant="{{.Variant}}"}[{{.Window}}]))
```

`{{.Namespace}}`, `{{.ModelDeployment}}`, `{{.Model}}` and `{{.Variant}}` expand to escaped contents for **double-quoted exact-match label strings**. They are not safe substitutions for arbitrary expression or regular-expression positions. `{{.Window}}` supplies the configured PromQL duration. Spaced forms such as `{{ .Namespace }}` are also supported. Adding `model="{{.Model}}"` to a comparison can exclude a stable variant serving an older model. The built-in usage-sample guard is not automatically applied to user-authored queries.

## Deprecations and upgrades

`llmcp_inference_output_tokens_total` remains temporarily available with its original **chunk-count** behavior; replace it with `llmcp_inference_output_chunks_total`, or explicitly choose reported-token totals with usage coverage checks. `llmcp_inference_tpot_seconds` remains a deprecated approximation: response duration after first content divided by remaining content chunks. It includes response-tail time and is not time per tokenizer token. Use observed inter-chunk latency for chunk timing. No removal release is set yet.

Upgrade the controller and shim as a matching build. Older shim images do not understand the required identity flags and cannot supply scoped series; a custom pinned image must be rebuilt or replaced. Built-in queries never fall back to model-only legacy data. Before enabling automatic decisions after an upgrade, verify the new labels at the metrics endpoint and in Prometheus and allow a complete fresh measurement window. Scoped identity isolates built-in decisions between resources; it is not an access-control boundary preventing custom PromQL from querying other resources.

Implementation references: [shim configuration](../cmd/shim/config.go), [collectors](../cmd/shim/metrics.go), [SSE/usage observer](../cmd/shim/sse.go), [query rendering](../internal/analysis/builtin.go), and [analysis evidence gates](../internal/analysis/analyzer.go).
