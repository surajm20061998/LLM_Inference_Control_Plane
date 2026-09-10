# ADR-0003: Inject a reverse-proxy sidecar to produce the SLIs the engine cannot

- **Status:** Accepted
- **Date:** 2026-09-03
- **Sprint:** 3

## Context

Every rollout decision this project makes — promote, hold, roll back, scale —
reads a latency percentile or an error rate. The engine has to supply them, and
llama.cpp does not.

`llama-server --metrics` exposes **gauges only**:

```
llamacpp:prompt_tokens_total          counter
llamacpp:tokens_predicted_total       counter
llamacpp:requests_processing          gauge
llamacpp:requests_deferred            gauge
llamacpp:kv_cache_usage_ratio         gauge
llamacpp:n_decode_total               counter
```

There is no histogram, no request-duration observation, no time-to-first-token,
and no HTTP status-code counter anywhere in that list. p95 latency and success
rate are therefore not *awkward* to compute from it — they are impossible. A
canary controller built directly on llama.cpp would have nothing to gate a
promotion on, and the honest version of this project would have to stop at
"deploys a model".

Four options were considered.

1. **Patch llama.cpp.** Correct upstream, wrong here: it forks the engine, welds
   the project to one server, and makes the "swap to vLLM" story a rewrite.

2. **Scrape the engine and derive percentiles from counters.** A counter of
   total tokens over a counter of total requests is a *mean*, and a mean is
   exactly the statistic a tail-latency regression hides in. Not viable.

3. **An OpenTelemetry collector as a sidecar.** It would need the same
   protocol-aware SSE parsing to find a first token, so the hard part is
   unchanged, and it adds a configuration surface and a second binary's
   lifecycle to the pod.

4. **A purpose-built reverse proxy as a native sidecar.**

## Decision

**Build `cmd/shim`: a reverse proxy injected as a native sidecar in front of
every engine, emitting `llmcp_*` as the project's canonical SLIs.**

By decree, `llmcp_*` is what analysis, autoscaling and alerting consume. Native
engine metrics — `llamacpp_*`, later `vllm:*` — are scraped as **diagnostics for
humans** and nothing automated is ever gated on them. One source of truth for
decisions, richer data for debugging.

Every canonical workload series carries the bounded identity labels
`namespace`, `model_deployment`, `model`, and `variant`. Automated selectors use
namespace and resource name so two deployments serving the same model cannot
share decision evidence. They deliberately omit the mutable model label during
a rollout because the stable primary may still serve the previous model.

The second reason is not observability at all, it is the engine abstraction.
`llmcp_inference_ttft_seconds` means the same thing under llama.cpp, vLLM or the
deterministic test stub, because one binary measures it in one place, one way.
Every dashboard, alert and rollout gate therefore survives an engine swap
unchanged — which is what makes `EngineSpec` a real abstraction rather than a
fiction, and it is why the shim is a Sprint 3 deliverable rather than a Sprint 6
nicety.

### Native sidecar, not an ordinary container

The shim is an entry in `initContainers` carrying `restartPolicy: Always`. Three
properties follow, and all three are needed:

- It **starts before** the engine, so there is no window in which traffic is
  served unmeasured.
- It **terminates after** the engine, so requests in flight during a pod
  shutdown still reach the error-rate counter instead of vanishing from it.
- Its **readiness counts toward pod readiness**, so a pod cannot join a Service
  with a dead proxy in front of a healthy engine.

As an ordinary container none of the three holds, and every failure they prevent
is silent.

### Three things that must be right, each of which fails silently

Each produces a plausible number rather than an error, which is why each is
called out in the code where it lives.

1. **`httputil.ReverseProxy{FlushInterval: -1}` and
   `Transport.DisableCompression: true`.** The default flush interval lets Go's
   `bufio` hold SSE frames until the buffer fills or the response ends; gzip is
   a block compressor and does the same thing from the other end of the
   connection. With either in place the client receives the whole generation at
   once and **every measured TTFT equals the total duration**. Nothing errors.
   The TTFT histogram simply becomes a duplicate of the duration histogram, and
   the canary gate is comparing a metric that no longer means what its name
   says.

2. **TTFT is taken at the first SSE frame carrying generated text.** Not the
   first byte — llama.cpp sends response headers as soon as it accepts the
   request — and not the first frame, which is typically a role-only opener with
   no text at all. Reasoning content counts: for a reasoning model it is the
   majority of the output, and skipping it would inflate TTFT by the length of
   the entire reasoning block.

3. **TTFT is never recorded for a non-streaming response.** There the first
   token and the last arrive together, so recording it enters the request's
   total duration into the TTFT histogram — a fabricated latency regression that
   a rollout gate would act on. Streaming is detected from the *response*
   `Content-Type`, not from the request's `"stream": true`, because the response
   is ground truth and needs no body buffering.

### Network shape

The serving Service targets the container port **by name** (`http`), which the
shim takes when injected and the engine keeps otherwise. Turning metrics on or
off therefore changes no client, no Service port and no endpoint URL.

Scraping goes through a **second, headless Service** (`<md>-metrics`). Two
reasons, and the second is the one that matters: the engine's own `/metrics`
must be reachable for the diagnostics scrape, and putting the engine's port on
the *serving* Service would let any in-cluster client address the engine
directly and bypass the proxy — at which point that traffic contributes to no
`llmcp_*` series and the canary analysis is reasoning about a subset of the load
while believing it sees all of it. Headless because a scraper wants one target
per pod; a ClusterIP VIP would land on a different pod every scrape, which for a
per-pod gauge like queue depth is meaningless rather than merely imprecise.

### Optional CRDs

`ServiceMonitor` is built as **unstructured** content and applied without a
startup informer. A manager asked to *watch* a kind whose CRD is absent fails at
`Start()` with `meta.NoKindMatchError` and does not recover when the CRD appears
later, because an informer's RESTMapper is resolved once. Building the object as
unstructured makes it structurally impossible to acquire a hard dependency on an
optional CRD.

`internal/discovery` probes for the kind with a TTL cache whose polarity is
asymmetric: 10 minutes for "present", 30 seconds for "absent". A negative answer
is the transient state of a cluster whose monitoring stack is still installing,
and caching it for long means a ModelDeployment created in that window never
gets a ServiceMonitor. A discovery *failure* is neither cached nor reported as an
absence — it produces `MetricsRegistered=Unknown` and a retry, because an API
server hiccup must not be reported to a user as "your monitoring is
uninstalled". The reconciler schedules a short retry while an enabled CRD is
absent and a bounded periodic resync while healthy, so installing the operator
later or deleting/drifting an owned monitoring child converges without editing
the ModelDeployment.

## Consequences

**Accepted:**

- One extra container per serving pod: ~15 MB RSS and a memory limit, with
  **deliberately no CPU limit**. A CPU limit would let the kubelet throttle the
  process sitting in the request path of every inference call, and time lost to
  that throttling is indistinguishable, in the resulting histogram, from the
  engine being slow — a canary gate would then roll back a release because its
  sidecar was squeezed.
- One extra network hop, over loopback inside a single pod.
- The shim and the controller are two halves of one build: the shim's metric
  names and identity labels are the contract the controller's analysis queries.
  A rollout performs a complete-window identity handshake before analysis;
  mismatched legacy shims hold under `MetricScopeReady=False` without spending a
  quality-verdict budget. `llmcp_shim_info` carries the shim's version so the
  skew is diagnosable.

**Rejected but reconsidered later:** a `router` mode for the same binary, doing
true per-request weighted backend selection. Held in reserve for the case where
replica-based traffic splitting proved too coarse. The spike measured the
keep-alive skew at 1.02 (on) and 1.09 (off) — well inside the noise — so
`trafficRouting.mode: Replica` stands and the router is not needed.

## Verification

The exit criterion is a single assertion, and it exists specifically so that
Sprint 4 does not build canary logic on a silently-broken TTFT:

```
ttft_p95 < 0.5 x duration_p95
```

in Prometheus, under live streaming load. All three failure modes above collapse
this ratio to 1.0, and there is no realistic middle ground for it to land in at
the project's default workload of ~64 output tokens.

- `make verify-ttft` asserts it against a live Prometheus and exits non-zero
  otherwise, so it can gate CI.
- `hack/loadgen` computes the same ratio client-side with no proxy in the path,
  which is what makes the check independent rather than self-referential.
- `TestProxyRecordsTTFTSeparatelyFromDuration` and
  `TestProxyStreamsIncrementally` assert it in unit tests, from the shim's side
  and the client's side respectively.
