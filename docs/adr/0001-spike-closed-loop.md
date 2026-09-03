# ADR-0001: Closed-loop feasibility spike — the TTFT signal is good enough to gate releases on

- **Status:** Accepted — **all six kill thresholds green**
- **Date:** 2026-09-02
- **Sprint:** 2 (ran alongside the real-engine work; gates Sprint 4)
- **Raw data:** [`data/0001/`](data/0001/) · **Reproduce:** `hack/spike/run-spike.sh`

## Context

Sprint 4 is the centrepiece of this project: canary releases promoted or rolled
back automatically on metric analysis. It is also the most expensive thing here
to build, and it rests entirely on one assumption that had never been tested —
that on a laptop, with a 0.6B model on CPU, **latency percentiles are stable
enough to set a threshold against**.

If they are not, the failure is not a slow demo. It is a controller that rolls
back healthy revisions at random, and an intermittent false rollback is far
harder to debug than a slow pod. Worse, the discovery would come *after* the
canary state machine, the analysis package and the metric providers were all
written against it.

So: six measurements, each with a kill threshold agreed in advance, run before
any Sprint 4 code exists.

## Environment

| | |
|---|---|
| Cluster | kind v0.33.0, `kindest/node:v1.35.8`, 1 control-plane + 2 workers |
| Host | Apple Silicon, 18 GiB / 11 CPU; Docker Desktop VM **11.67 GiB / 8 CPU** |
| Engine | `ghcr.io/ggml-org/llama.cpp:server-b10731` |
| Model | Qwen3-0.6B-Q4_K_M (397 MB), delivered as an OCI image per [ADR-0002](0002-model-weight-delivery.md) |
| Primary | 2 replicas, `cpu: 2` → `-t 2`, `--parallel 4`, `-c 4096` |
| Canary | 1 replica, `cpu: 500m` → `-t 1`, otherwise identical |
| Load | `hack/loadgen`, streaming, 64 max tokens, greedy, thinking disabled |

**The regression is induced by CPU limit, not by an injected sleep.** The plan
called for a throwaway sidecar with `LLMCP_INJECT_LATENCY_MS=400`, but that
sidecar does not exist until Sprint 3. Halving the canary's CPU is arguably the
better test anyway: a synthetic sleep adds a constant offset, whereas CPU
starvation degrades exactly the prompt-processing and decode work that real
latency metrics measure. It is what a genuinely bad rollout looks like.

## Results

| # | Measurement | Kill threshold | Result | |
|---|---|---|---|---|
| 1 | Peak per-pod memory | > 2048 MiB | **938 MiB** | PASS |
| 2 | Sustained req/s, canary | < 0.5 req/s | **1.27 req/s** | PASS |
| 3 | Separation, p95 TTFT canary vs primary | < 2.0× | **4.54×** | PASS |
| 4 | Noise, CV of p95 TTFT over 15 windows | > 0.5 | **0.204** | PASS |
| 5 | Traffic split, keep-alive on vs off | off differs > 2× | **1.02 vs 1.09 skew** | PASS |
| 6 | Cold start to a passing `/health` | > 90 s | **9 s** | PASS |

Client-side error rate was **0.0%** across every run.

### 1 — Memory: less than half the budget

Peak `memory.current` was 938 MiB per pod, of which 933 MiB was anonymous. Three
replicas plus the control plane sit comfortably inside an 11.67 GiB VM. The
2 GiB per-pod limit in the samples has roughly 2× headroom, so it is a guard
rail rather than a constraint.

### 3 — Separation: large, and clearer on duration than on TTFT

Like-for-like, one pod per side, four concurrent requests each:

| | primary (`cpu: 2`) | canary (`cpu: 500m`) | ratio |
|---|---|---|---|
| p95 TTFT | 0.059 s | 0.267 s | **4.54×** |
| p95 duration | 1.108 s | 4.125 s | **3.72×** |
| throughput | 4.60 req/s | 1.27 req/s | 0.28× |

Both signals separate the variants decisively. This comparison had to be made
carefully, and the first attempt got it wrong — see *What nearly went wrong*.

### 4 — Noise: 20%, against an effect size of 354%

Fifteen 10-second windows at the realistic two-pod shape gave per-window p95
TTFT ranging 0.44–0.92 s, CV **0.204**. Throughput over the same windows was
6.5–8.0 req/s, so the variation is latency jitter rather than load drift.

The number that matters is the ratio of the two: a regression that moves p95
TTFT by **354%** against a noise floor of **20%** is roughly a 17-sigma effect.
There is a very wide band in which to place a threshold.

### 5 — Traffic split: the open risk did not materialise

The plan flagged this as the one thing that could quietly invalidate the whole
demo: kube-proxy balances **per connection**, so N keep-alive clients could pin
themselves to N pods and stay there, making a canary's realized traffic share an
arbitrary multiple of 1/N — stable and wrong for an entire canary window.

Measured share of prompt tokens per pod:

| Shape | Keep-alive | Split | Skew (max/min) |
|---|---|---|---|
| 2 pods, 8 connections | on | 49.5 / 50.5 | **1.02** |
| 2 pods, 8 connections | off | 52.2 / 47.8 | **1.09** |
| 4 pods, 8 connections | on | 23.8 / 23.9 / 24.8 / 27.5 | **1.16** |

Keep-alive on and off are within 7% of each other — nowhere near the 2×
threshold. The four-pod case was added because two pods and eight connections
can come out even by luck; four pods is where pinning would show as a lumpy
split, and it did not.

**Why pinning did not bite:** cpp-httplib closes a connection after
`keep_alive_max_count` requests (100 by default). Over a 90-second run each
connection is recycled several times and lands on a fresh pod each time, so
placement re-randomises continuously and the aggregate split converges. This is
a property of llama.cpp, not of Kubernetes — an engine that held connections
indefinitely would behave differently, and that is worth re-testing when the
vLLM profile lands in Sprint 8.

**An important caveat, though.** An even split *in aggregate over a run* does
not mean load is even *at any instant*. The same primary measured at 2 pods /
8 connections showed p95 TTFT of 0.72 s, against 0.059 s at 1 pod / 4
connections — 12× higher, from transient moments where one pod held more than
the four concurrent requests llama.cpp has slots for, and the excess queued.
**Service-level p95 TTFT therefore contains connection-placement queueing, not
just engine health.** Sprint 4's analysis must compare canary against primary
under the same conditions rather than against an absolute number, which is what
`thresholdRange` on a *ratio* already does — this measurement is the
justification for that choice rather than a change to it.

### 6 — Cold start: 9 seconds

From pod deletion to every replacement passing readiness — which for this
operator means llama.cpp's own `/health` returned 200, so the model is resident
and slots are available. Weights come from a node-local image, so this is mmap
plus warmup, not a download.

## Decision

**Build Sprint 4 as planned.** Every kill threshold is green, most of them by a
wide margin. Specifically:

- **llama.cpp stays the primary demo path.** The fallback in the plan — promote
  the fake engine to the demo and relegate the real engine to a footnote — is
  not needed.
- **Keep `trafficRouting.mode: Replica`.** The ~150-line shim `router` mode held
  in reserve for a red result on measurement 5 is **not** required. Weight
  quantization is still reported honestly (`desiredWeight: 20` vs
  `currentWeight: 33`), because that is about weight *granularity* at small
  replica counts, which is a separate and still-real concern.
- **`--disable-keepalive` is downgraded from a required mitigation to a
  documented option.** It changed the split by 7%.

### Defaults set by these measurements

Per the plan's rule of thumb — an analysis window should cover at least ~100
requests per variant:

| Field | Value | Derivation |
|---|---|---|
| `analysis.interval` | **30s** | With `failureThreshold: 3`, a bad canary rolls back in ~90–120 s: fast enough to demo, slow enough that one unlucky window cannot trigger it |
| `analysis.window` | **60s** | ~276 requests for a healthy canary at 4.6 req/s, ~76 for one degraded to 1.27 req/s. Comfortably above ~100 in the case that matters, and adequate in the degraded case where the effect size is 4.5× anyway |
| `minRequestRate` | **0.5 req/s** | Below the degraded canary's measured 1.27 req/s, above zero. A canary receiving no traffic yields `Inconclusive`, never a free `Pass` |

A threshold on p95 TTFT at roughly **2× the primary's** sits about 5 standard
deviations above the noise floor and about half the observed regression — clear
of both.

## What nearly went wrong

Three methodology bugs were found and fixed while running this. Each produced
confident, plausible, wrong numbers, and each is recorded because the same
mistakes would be easy to repeat in Sprint 3 and 4.

**1. Load driven through `kubectl port-forward`.** It reported a **50% error
rate**, every failure a bare `EOF`, with the engine pods entirely healthy.
port-forward multiplexes connections over a single stream and drops them under
concurrency. It is also wrong in principle: it resolves a Service to *one* pod,
so kube-proxy is never in the path and measurement 5 would have been measuring
`kubectl`. Fixed by running the load generator **inside the cluster** as a Job
(`Dockerfile.loadgen`).

**2. A confounded separation test.** Comparing a 2-pod primary at concurrency 8
against a 1-pod canary at concurrency 4 produced an **inverted** result: the
healthy primary appeared *slower* on p95 TTFT (0.72 s) than the CPU-starved
canary (0.27 s). The cause is the queueing effect described under measurement 5
— p95 was measuring queueing on one side and pure compute on the other. Had this
been taken at face value, Sprint 4 would have been built on the belief that TTFT
does not discriminate. Fixed by scaling the primary to one replica for that
phase, so the CPU limit is the only difference.

**3. A residual few-percent error rate**, all `server closed idle connection`.
cpp-httplib closes keep-alive connections after its max count while Go's
transport still holds them (default `IdleConnTimeout` 90 s), and `net/http`
will not retry a POST unless the caller marks it replayable. Fixed with a 2 s
`IdleConnTimeout` and an `Idempotency-Key` header. **Error rate is one of the
signals canary analysis gates on**, so a spurious floor under it is not
something to tolerate — a 3% client-side artifact would sit permanently under
every `error-rate` check in Sprint 4.

**4. A cold-start measurement of "0 seconds."** The pods were already running
from an earlier run, so `kubectl apply` was a no-op and the readiness wait
returned instantly. Fixed by deleting the pods and timing their replacements.

## Consequences

- Sprint 4 is unblocked and proceeds as designed.
- `hack/loadgen` is retained rather than thrown away. It measures TTFT from
  outside the cluster with no proxy in the path, which is exactly what Sprint 3
  needs to check the shim's histogram against something independent — the
  `FlushInterval: -1` bug makes every TTFT equal total duration while still
  looking entirely plausible. The `ttft_p95_over_duration_p95` field exists for
  that check and read **0.05** here, against the 0.5 ceiling Sprint 3 must meet.
- The numbers above are per-pod at `cpu: 2` on this specific host. They set
  defaults, not invariants; CI runs against the fake engine with compressed
  intervals precisely so that none of this has to be reproducible on a runner.
- Re-run measurement 5 when the vLLM profile lands. The favourable result
  depends on cpp-httplib's connection recycling, which is llama.cpp's behaviour
  and not a guarantee.
