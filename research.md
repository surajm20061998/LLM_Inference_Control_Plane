# Mini LLM Inference Control Plane — Technology Research

**Research date:** 2026-09-01
**Target environment:** macOS (darwin 25.6.0), Apple Silicon **arm64**, 18 GB RAM, 11 CPUs
**Scope:** what to build a production-grade Go Kubernetes operator on, in 2026, for a `ModelDeployment` CRD that deploys an LLM inference server with health checking, autoscaling, rolling updates, canary releases, automated rollback, and Prometheus/Grafana observability — running locally on kind against a tiny CPU model.

## How to read this document

Three parallel research passes were run against primary sources (GitHub Releases API, official docs, registry manifest APIs, Hugging Face model cards) rather than model training data, because container tags, arm64 support, and project maintenance status all move fast and most secondary tutorials are stale.

Everything in §1–§9 was verified against a primary source. **§10 lists what could not be verified and where sources actively conflict** — read it before relying on anything marked ⚠️.

Facts marked **✅ re-verified** were checked a second time, directly, at the terminal (`docker manifest inspect`, Docker Hub v2 API, GitHub Releases API, raw `Chart.yaml`) rather than being taken on trust from the research pass. Those are the load-bearing claims the whole architecture rests on.

---

## 1. Version baseline

### 1.1 Local environment as found

| Tool | Version | Note |
|---|---|---|
| Go | 1.26.2 darwin/arm64 | Meets controller-runtime v0.24's Go ≥1.26 floor |
| kind | v0.31.0 | **Behind** — v0.33.0 is current; tops out at node image v1.35.0 |
| kubectl | v1.35.0 (kustomize v5.7.1) | |
| Helm | v4.1.0 | |
| Docker | CLI 29.2.0 | **Daemon was not running** at time of research |
| kubebuilder | not installed | |
| Hardware | arm64, 11 CPUs, 18 GB | |

Docker Desktop's Linux VM has **no GPU/Metal access**. Everything inside kind is CPU-only `linux/arm64`. MLX, `vllm-metal`, and any Apple-Silicon-native inference path are therefore irrelevant to this project, however correct they are for native macOS.

### 1.2 Verified upstream versions (✅ re-verified via GitHub Releases API, 2026-09-01)

| Project | Latest | Published |
|---|---|---|
| kubernetes-sigs/kubebuilder | **v4.15.0** | 2026-06-15 |
| kubernetes-sigs/kind | **v0.33.0** | 2026-08-26 |
| kubernetes-sigs/controller-runtime | **v0.24.1** | 2026-05-12 |
| kubernetes-sigs/controller-tools | **v0.21.0** | 2026-05-06 |
| kubernetes-sigs/gateway-api | **v1.6.1** | 2026-07-16 |
| kedacore/keda | **v2.20.2** | 2026-07-31 |
| fluxcd/flagger | **v1.45.0** | 2026-09-01 |
| argoproj/argo-rollouts | **v1.10.0** | 2026-08-27 |
| kyverno/chainsaw | **v0.2.15** | 2026-05-06 |
| envoyproxy/gateway | **v1.9.1** | 2026-08-28 |
| prometheus-operator | **v0.93.1** | 2026-08-10 |
| golangci-lint | v2.13.2 | 2026-08-27 |
| cert-manager | v1.21.1 | 2026-07-29 |
| elastic/crd-ref-docs | v0.3.0 | 2026-02-02 |
| vLLM | v0.28.0 | 2026-08-26 |
| Prometheus | v3.14.0 (LTS line 3.13) | 2026-08-17 |

### 1.3 The controller-runtime ↔ Kubernetes ↔ Go triple

This is the single most consequential pinning decision. From the [controller-runtime compatibility matrix](https://github.com/kubernetes-sigs/controller-runtime/blob/main/README.md):

| controller-runtime | `k8s.io/*` + client-go | Min Go |
|---|---|---|
| **v0.24** | **v0.36** | **1.26** |
| v0.23 | v0.35 | 1.25 |
| v0.22 | v0.34 | 1.24 |

Kubebuilder v4.15.0 scaffolds onto controller-runtime v0.24.1 / k8s v0.36 / Go 1.26. Installed kind v0.31.0 cannot boot a 1.36 node, which is the conflict to resolve.

**Resolution: upgrade kind to v0.33.0 and pin `kindest/node:v1.35.8`.** kind v0.33.0 ships node images v1.37.0, v1.36.4, **v1.35.8**, v1.34.11. ✅ Re-verified: `kindest/node:v1.35.8` publishes both `linux/amd64` and `linux/arm64` manifests. client-go tolerates ±1 minor skew, so CR v0.24 against a 1.35 apiserver works in practice; staying on 1.35.x keeps envtest binaries and generated CRD schemas aligned with a well-trodden version.

Also relevant: **Kubernetes 1.35+ removes cgroup v1 support**, and kind v0.32.0 replaced HAProxy with Envoy for its load balancer and moved containerd config to the v4 format — so any registry-setup script written for kind ≤0.31 needs re-checking after the upgrade.

Sources: [kind releases](https://github.com/kubernetes-sigs/kind/releases) · [controller-runtime releases](https://github.com/kubernetes-sigs/controller-runtime/releases) · [Kubernetes v1.35 release](https://kubernetes.io/blog/2025/12/17/kubernetes-v1-35-release/)

---

## 2. Operator scaffolding and framework

### 2.1 Kubebuilder over Operator SDK over raw controller-runtime

**Recommendation: raw Kubebuilder v4.15.0.**

- **Operator SDK uses Kubebuilder + controller-runtime underneath for Go operators.** It only adds value for Ansible/Helm-based operators or OLM bundle packaging. For a Go operator it is an indirection layer for nothing. ([Operator SDK FAQ](https://sdk.operatorframework.io/docs/faqs/))
- **Raw controller-runtime** is defensible for a tiny controller, but you hand-write RBAC markers, CRD generation wiring, the Makefile, envtest setup, and the Dockerfile. For a project whose point is production-grade engineering, the scaffold is itself part of the signal.

Recent Kubebuilder timeline (GitHub API is authoritative; a web search result claiming v4.13.0 shipped 2026-03-06 contradicts it — trust the API):

| Version | Published | Highlights |
|---|---|---|
| **v4.15.0** | 2026-06-15 | k8s 1.36 + Go 1.26; CR v0.23.3 → **v0.24.1**; controller-tools v0.20.1 → **v0.21.0**; **pins the ENVTEST version in generated projects**; optional NetworkPolicy in the Helm chart |
| v4.14.0 | 2026-04-30 | Multiple controllers per GVK; Helm plugin `v2-alpha` stabilization; extra volumes, deployment strategies, priorityClass |
| v4.13.0 | 2026-02-27 | File permissions standardized; env-var overrides for Helm |
| v4.12.0 | 2026-02-16 | `--namespaced` flag; multigroup init; custom k8s logging lint rules |

### 2.2 What the v4.15.0 scaffold produces

- **Layout:** `api/<group>/<version>/`, `internal/controller/`, `cmd/main.go`, `config/` (kustomize v5.8.1), `test/e2e/`, `test/utils/`, `Dockerfile`, `Makefile`, `.golangci.yml`, DevContainer config.
- **Metrics — the big change versus every older tutorial:** `kube-rbac-proxy` is **gone**. Not scaffolded since v3.15.0; since **v4.1.0** the metrics endpoint is enabled and protected by controller-runtime's built-in `FilterProvider: filters.WithAuthenticationAndAuthorization` in `metricsserver.Options`. The `gcr.io/kubebuilder/kube-rbac-proxy` image is being retired entirely. ([design doc](https://github.com/kubernetes-sigs/kubebuilder/blob/master/designs/discontinue_usage_of_kube_rbac_proxy.md) · [metrics reference](https://book.kubebuilder.io/reference/metrics))
- **cert-manager:** wiring for webhook and metrics TLS is scaffolded **commented out** since v4.4.0, deliberately, to avoid the dependency.
- **Helm plugin:** `kubebuilder edit --plugins=helm/v2-alpha` generates a chart *from* the kustomize output — no duplication. Keep kustomize as source of truth, ship the chart as the distribution artifact.
- **AutoUpdate plugin:** `autoupdate.kubebuilder.io/v1-alpha` scaffolds a weekly GitHub Actions job running `kubebuilder alpha update`.

### 2.3 Breaking changes that invalidate most published tutorials

**v0.24.0 (2026-04-30):** bump to k8s v0.36; **min Go 1.26**; removed the deprecated webhook custom-path function. **Use v0.24.1, not v0.24.0** — v0.24.0 shipped an `Apply` typed-error regression fixed in the patch.

**v0.23.0 (2026-01-19)** — these three break almost every blog post:
1. **Events migrated to the `events.k8s.io` API group.** `GetEventRecorderFor` now emits there, so RBAC must grant on `events.k8s.io`. Forget it and **the event recorder fails silently** — no error, no events.
2. **Webhook builders now require concrete types via generics:** `builder.WebhookManagedBy(mgr).For(&T{})` became `builder.WebhookManagedBy(mgr, &T{})`. `CustomValidator`/`CustomDefaulter` are generic.
3. **Priority queue enabled by default** — tests assuming FIFO reconcile ordering will flake.

**Reconcile semantics** (verified against [`pkg/reconcile/reconcile.go`](https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/reconcile/reconcile.go)):

```go
type Result struct {
    Requeue      bool          // DEPRECATED — "causes confusion and there is no good reason to use it"
    RequeueAfter time.Duration
    Priority     *int          // new; honored now that the priority queue is default-on
}
```

`reconcile.TerminalError(err)` suppresses the retry while still logging and recording metrics — the correct response to an invalid spec the API server could not reject, so the controller doesn't hot-loop on unfixable input.

---

## 3. CRD API design in 2026

### 3.1 Status conditions

Per [SIG-Architecture API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md): use `[]metav1.Condition` with `+patchMergeKey=type`, `+patchStrategy=merge`, `+listType=map`, `+listMapKey=type`. Condition `Type` is a CamelCase adjective or past-tense verb (`Ready`, `Available`, `Progressing`, `Degraded`) — **not** a present-tense verb like `Deploying`. `Reason` is **required** and must be a one-word CamelCase programmatic identifier. Report conditions on first visit even as `Unknown`, to signal active reconciliation. Use `apimeta.SetStatusCondition` / `FindStatusCondition`. Carry `status.observedGeneration` at the top level and gate "is this rollout done?" on `observedGeneration == metadata.generation`.

Getting the list semantics wrong causes SSA conflicts and lost updates on the conditions array — a subtle and expensive bug.

### 3.2 CEL validation — GA, and it removes the need for webhooks

`x-kubernetes-validations` went **GA in Kubernetes v1.29** ([KEP-2876](https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/2876-crd-validation-expression-language)), so it is fully available on a 1.35 target with **zero infrastructure, zero certificates, zero failure modes**. This is the single biggest reason a modern operator can defer admission webhooks entirely.

Marker: `+kubebuilder:validation:XValidation:rule="...",message="..."`. Transition rules use `oldSelf` (with `optionalOldSelf` for correct create-time behavior).

**Newer relational markers** ([CRD validation markers reference](https://book.kubebuilder.io/reference/markers/crd-validation)) that did not exist a couple of years ago and are ideal for a canary API:

- `+kubebuilder:validation:ExactlyOneOf` — perfect for union types (a model-source union, or Argo-style canary steps) **without** a discriminator field and without hand-written CEL
- `+kubebuilder:validation:AtMostOneOf` / `AtLeastOneOf`
- `+k8s:immutable` — declarative immutability
- `+kubebuilder:title`, `+kubebuilder:example` — feed OpenAPI and generated docs
- Plain `+required` / `+optional` are now preferred over `+kubebuilder:validation:Required`

**CRD ratcheting** ([KEP-4008](https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/4008-crd-ratcheting)) is beta-and-default-on since v1.30: an update failing validation is accepted if every failing keypath was unchanged. This is what makes it safe to tighten validation in a future `v1alpha2` without breaking existing objects.

### 3.3 The `scale` subresource

```yaml
subresources:
  status: {}
  scale:
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .status.selector    # REQUIRED for HPA
```

Marker: `+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector`

HPA uses the polymorphic scale client: it `GET`s `/scale` and `PUT`s an `autoscaling/v1.Scale`. Requirements the controller must satisfy:

- `spec.replicas` as **`*int32`** (a pointer, so "unset" is distinguishable — critical when an HPA owns the field)
- `status.replicas` kept **accurate**, or HPA computes garbage
- `status.selector` as a **serialized selector string** via `metav1.LabelSelectorAsSelector(sel).String()` — **not** a `metav1.LabelSelector`. This is the #1 documented bug in CRD scale implementations ([strimzi#3432](https://github.com/strimzi/strimzi-kafka-operator/issues/3432), [k/k#66688](https://github.com/kubernetes/kubernetes/issues/66688))
- RBAC on `<resource>/scale` with `get;patch;update`

**The design tension to resolve explicitly:** if HPA writes `spec.replicas` while the controller also owns replica count during a canary, they fight. Argo Rollouts' resolution: HPA scales the *total*, and the controller derives the stable/canary split from it. Adopt the same contract.

### 3.4 Server-Side Apply and generated applyconfigurations

controller-runtime v0.24.1 provides `client.Apply` taking `runtime.ApplyConfiguration`, `ApplyOptions{DryRun, Force, FieldManager}`, `client.FieldOwner(...)`, `client.ForceOwnership`, and **`SubResourceWriter.Apply`** (SSA on status, new in v0.23).

Per the [Kubernetes SSA guidance](https://kubernetes.io/blog/2022/10/20/advanced-server-side-apply/), a controller should rebuild the full apply configuration for objects it owns on every reconcile, use a stable unique field manager, and set `Force`. The applied object must contain *every* field the controller cares about — omitted fields are released.

That release semantic is the real payoff: **omitting `spec.replicas` from the applied Deployment declares "I do not own this field,"** which is exactly what makes coexistence with an autoscaler declarative rather than hand-coded read-then-preserve logic.

**controller-tools v0.21.0 does generate applyconfigurations for CRDs** ("Generate extract functions", "support for external ApplyConfiguration mappings", cluster-scoped fix). Invoke as `controller-gen applyconfiguration:headerFile=hack/boilerplate.go.txt paths=./api/... output:dir=./api/v1alpha1/ac`. ⚠️ It is **not** wired into the default Kubebuilder Makefile, and the runtime side needs a `managedfields.TypeConverter` built from the CRD's OpenAPI schema — non-trivial for a CRD, where client-go doesn't ship the parser as it does for built-ins.

**Assessment:** use SSA with client-go's built-in applyconfigurations for children (free, immediate payoff). Generating applyconfigurations for your own CRD only pays off if two controllers write disjoint parts of one status; with a single reconciler, `Status().Update()` + `RetryOnConflict` is ten lines and a well-understood failure mode.

**The SSA trap:** SSA + `Owns()` produces an infinite reconcile loop if the applied object is not byte-stable. Causes: iterating a Go map to build env vars or labels (always sort keys), emitting `resources: {}` versus omitting it, and emitting fields the apiserver defaults differently (`terminationMessagePath`, `dnsPolicy`, `schedulerName`). The cheap defense is an integration test that reconciles twice and asserts the child's `resourceVersion` is unchanged on the second pass.

### 3.5 Watches, predicates, ownership

- `Owns(&appsv1.Deployment{})` for children — owner refs give garbage collection for free.
- `Watches(..., handler.EnqueueRequestsFromMapFunc(...))` for signals that don't flow through owner refs.
- `predicate.GenerationChangedPredicate{}` on the **primary** resource so status writes don't self-trigger — **but never on an owned Deployment's watch.** You specifically need status-only changes (`readyReplicas`) to wake you up, and that predicate filters exactly those out. This is the most common operator bug in the wild.
- Cross-namespace owner refs do not work; cluster-scoped owners of namespaced objects do not work.

**On finalizers:** they earn their place only if there is genuine external cleanup (draining in-flight requests, deregistering from a gateway). Otherwise owner-ref GC suffices, and a stuck finalizer is a worse production signal than no finalizer.

---

## 4. Inference engines on CPU / arm64

This section drove the largest correction to prior assumptions, in both directions.

### 4.1 llama.cpp — arm64 images exist; older sources saying otherwise are stale

✅ **Re-verified by direct manifest inspection on 2026-09-01:**

```
$ docker manifest inspect ghcr.io/ggml-org/llama.cpp:server
linux amd64  sha256:3b99319913211929...
linux arm64  sha256:0ab379e88348888e...     ← confirmed present
linux s390x  sha256:611c897eae1ea...
```

This matters because there is **directly conflicting evidence in the public record**. [Issue #13891](https://github.com/ggml-org/llama.cpp/issues/13891) and [issue #19177](https://github.com/ggml-org/llama.cpp/issues/19177) (re-reported 2026-01-29) both report the arm64 image missing despite the docs claiming otherwise. It was fixed by **[PR #21207, merged 2026-03-31](https://github.com/ggml-org/llama.cpp/pull/21207)**, which added ARM64 matrix entries for the CPU and Vulkan release builds. **Any source older than April 2026 saying "no arm64 llama.cpp image" is stale.** One of the two research passes reproduced that stale conclusion from the open issues; the direct manifest check settled it.

**Pin a build, not `:server`** — the `:server` tag moves ~10×/day. Build-suffixed tags (`server-b10731`) are published.

Build matrix (from `.github/workflows/docker.yml`): the `cpu` target (→ `full`/`light`/`server`) builds `linux/amd64`, `linux/arm64`, `linux/s390x`. CUDA and Vulkan build amd64+arm64; `musa`, `intel`, `rocm`, `openvino` are amd64-only.

**Endpoints** (from [`tools/server/README.md`](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)):

| Path | Behavior |
|---|---|
| `GET /health` | **Public, no API key. 503 + `{"error":{"message":"Loading model"}}` while loading; 200 + `{"status":"ok"}` when ready.** Genuinely correct readiness semantics — rarer than it should be. |
| `GET /metrics` | Prometheus text format. **Requires the `--metrics` flag.** |
| `POST /v1/chat/completions`, `POST /v1/completions`, `GET /v1/models` | OpenAI-compatible, streaming + tool use |
| `GET /props` | context size, default gen settings, model path |
| `GET /slots` | per-slot state; `?fail_on_no_slot=1` → 503 when saturated |

**Flags that matter:** `--host 0.0.0.0` (default is `127.0.0.1` — **must** be overridden in k8s), `--port` (default 8080), `--metrics`, `-hf <repo>[:<quant>]`, `-m <path>`, `-c <ctx>`, `-t <threads>`, `--parallel N`, `--api-key`.

**Metrics exposed** (verified):

```
llamacpp:prompt_tokens_total              llamacpp:tokens_predicted_total
llamacpp:prompt_seconds_total             llamacpp:tokens_predicted_seconds_total
llamacpp:prompt_tokens_seconds            llamacpp:predicted_tokens_seconds
llamacpp:requests_processing   (gauge — active requests)
llamacpp:requests_deferred     (gauge — QUEUED requests)
llamacpp:n_tokens_max                     llamacpp:n_decode_total
llamacpp:n_busy_slots_per_decode
llamacpp:spec_decode_num_draft_tokens_total  (+3 more spec-decode counters)
```

**🚨 The critical gap: llama.cpp emits no histograms.** No request-duration histogram, no TTFT, no HTTP status-code counter. `histogram_quantile` p95/p99 and success-rate PromQL are therefore **impossible from llama.cpp alone**. This single fact determines the architecture — see §7.4.

**Two known metric defects worth citing:**
- [Issue #12803](https://github.com/ggml-org/llama.cpp/issues/12803) — the `llamacpp:foo` names are non-conformant; **Prometheus reserves `:` for recording rules**, and exporters must not emit it. Most scrapers tolerate it, but strict tooling and recording-rule setups will choke. Mitigate with a ServiceMonitor `metric_relabel_configs` rewriting `llamacpp:(.*)` → `llamacpp_$1`.
- [Issue #19811](https://github.com/ggml-org/llama.cpp/issues/19811) "Prometheus metrics need improvement" and [#17384](https://github.com/ggml-org/llama.cpp/issues/17384) "Metrics for prometheus now incorrectly formatted" ⚠️ — not read in full; check whether #17384 affects the pinned build.

**Operational gotcha:** llama.cpp reads the host `/proc` CPU count and is **cgroup-unaware**. With N replicas on an 11-CPU VM it spawns 11×N threads and thrashes, making p95 pure noise. `-t` must be derived explicitly from the container's CPU limit.

### 4.2 vLLM on CPU/arm64 — official prebuilt images now exist

✅ **Re-verified via the Docker Hub v2 API on 2026-09-01.** Repository `vllm/vllm-openai-cpu` (note: **not** `vllm/vllm-openai`, which is CUDA-only):

| Tag | Arch | Compressed | Updated |
|---|---|---|---|
| `vllm/vllm-openai-cpu:latest` / `:v0.28.0` | multi-arch amd64+arm64 | — | 2026-08-26 |
| `vllm/vllm-openai-cpu:v0.28.0-arm64` | arm64 | ~875 MB | 2026-08-26 |
| `vllm/vllm-openai-cpu:v0.27.1-arm64` | arm64 | ~871 MB | 2026-08-11 |
| `vllm/vllm-openai-cpu:latest-x86_64` | amd64 | ~1.78 GB | 2026-08-26 |

arm64 tags exist back to at least v0.22.1 (2026-06-05). **Building vLLM from source for arm64 CPU is no longer necessary.**

From `docs/getting_started/installation/cpu.arm.inc.md`:
- ISA: **NEON required**. Supported dtypes on ARM: **FP32, FP16, BF16**.
- ⚠️ **BF16 requires ARMv8.6-A (FEAT_BF16).** Apple **M1 is ARMv8.5-A and lacks it**; M2/M3/M4 have it. On M1 use `--dtype=float32` or `float16`.
- Prebuilt wheels for Arm exist since v0.11.2.
- Testing baseline is AWS Graviton3, **not** Apple Silicon.

**Two real footguns:**
- **`--gpu-memory-utilization` is still honored on CPU and defaults to 0.92** — it will try to reserve 92% of VM RAM. Must be set explicitly. This is a genuine OOM hazard on an 18 GB Mac.
- **Never pull `vllm/vllm-openai`** (no `-cpu`). It has an arm64 manifest (~10.2 GB) that pulls fine and then fails at runtime looking for a CUDA driver the Docker VM does not have.

Other environment knobs: `VLLM_CPU_KVCACHE_SPACE` (GiB reserved for KV cache, defaults to 4), `VLLM_CPU_OMP_THREADS_BIND`. Some setups need `--security-opt seccomp=unconfined --cap-add SYS_NICE --shm-size=4g`.

**V1 engine:** V0 is fully removed ("We have fully deprecated V0", RFC #18571) and the V1 hardware matrix lists **CPU as fully functional**. The old blocker [issue #19461](https://github.com/vllm-project/vllm/issues/19461) ("CPU docker image does not support vLLM v1") is historical.

**Metrics** (V1 names, verified):

```
vllm:num_requests_running          gauge     requests in model execution batches
vllm:num_requests_waiting          gauge     QUEUE DEPTH
vllm:kv_cache_usage_perc           gauge     1.0 == 100%
vllm:time_to_first_token_seconds   HISTOGRAM
vllm:e2e_request_latency_seconds   HISTOGRAM
vllm:inter_token_latency_seconds   HISTOGRAM
vllm:prompt_tokens                 counter
vllm:generation_tokens             counter
vllm:request_success               counter
vllm:num_preemptions               counter
vllm:prefix_cache_hits / _queries  counter
```

**🚨 Naming trap:** `vllm:gpu_cache_usage_perc` is the **V0** name. In V1 it is **`vllm:kv_cache_usage_perc`** — device-agnostic, which is also why it is meaningful on the CPU backend. Google's GKE docs still reference the stale name in places.

vLLM's metric deprecation policy: metrics hidden in X.Y+1 are removed in X.Y+2, so PromQL should be pinned to a version range.

**Performance** ⚠️ single third-party source: a [2026 tutorial](https://recatools.com/guides/how-to-serve-llm-vllm-cpu-docker/) reports ~12.5 tok/s serving Qwen3-0.6B on Apple Silicon Docker with 6 CPUs / 7.75 GiB, self-described as "not a benchmark". Image unpacks to ~3.58 GB. Startup is minutes (torch.compile + weight load).

**Verdict:** viable and worth shipping as a second engine profile for prod-parity, but too heavy and too slow-starting to be the default for a demo that does repeated rollouts.

### 4.3 Engines evaluated and rejected

| Engine | Status 2026 | Why rejected |
|---|---|---|
| **SGLang** | CPU backend requires **Intel AMX** (Sapphire Rapids+, x86-only). Images are `lmsysorg/sglang:*-xeon`. The CPU docs do not mention ARM64 at all. | No arm64 path. ⚠️ Arm Ltd published work on [SGLang for Arm Neoverse](https://developer.arm.com/community/arm-community-blogs/b/ai-blog/posts/bringing-sglang-high-performance-llm-inference-to-arm-neoverse) (~PR #22123, May 2026) targeting Neoverse server cores, but no `lmsysorg/sglang:*-arm64` tag was found. Research-grade. |
| **HuggingFace TGI** | README on `main`: *"text-generation-inference is now in maintenance mode."* Supported hardware lists NVIDIA/AMD/Inferentia/Intel/Gaudi/TPU — **no arm64**. README on CPU: *"CPU is not the intended platform."* | Dead end; HF now points users to vLLM or SGLang. |
| **Ollama** | **No native `/metrics` endpoint.** [Issue #3144](https://github.com/ollama/ollama/issues/3144) is still the open tracking issue; the community pattern is a sidecar exporter polling `/api/ps`. Health is effectively `GET /` returning a string — no model-load readiness semantics. Lazily loads/unloads models, fighting k8s readiness. | **Kills metrics-driven autoscaling entirely.** ⚠️ Some 2026 blogs claim Ollama exposes `/metrics`; that is not in the upstream repo or docs. |
| **LocalAI** | Multi-backend OpenAI-compatible | Heavier and broader than needed |
| **llamafile** | Single-file Cosmopolitan binary | Awkward in k8s/multi-arch |

### 4.4 Recommendation

**Default: `ghcr.io/ggml-org/llama.cpp:server-b<pinned>`.** It is the only option with all four of: verified multi-arch arm64 image, small size (~100–300 MB), a *real* `/health` with 503-during-load, and native `/metrics`. Start time is seconds, not minutes — essential when the operator does repeated rolling updates and canaries on a laptop.

**Second engine: `vllm/vllm-openai-cpu:v0.28.0-arm64`** — used to prove the engine abstraction is real, on the same code path that would run `vllm/vllm-openai` + `nvidia.com/gpu` on a GPU cluster.

The CRD should be engine-agnostic: both expose `/health`, `/metrics`, `/v1/models`, `/v1/chat/completions`, `/v1/completions`. Only image, args, and native metric names differ — a clean `EngineProfile` interface seam.

---

## 5. Models

### 5.1 Licensing and gating

| Model | License | Gated? | Notes |
|---|---|---|---|
| **`Qwen/Qwen3-0.6B`** | **Apache-2.0** | **No** | 0.6B total / 0.44B non-embedding, 28 layers, GQA 16Q/8KV, **32,768 ctx**. ~1.5 GB bf16. **Best default.** |
| `Qwen/Qwen2.5-0.5B-Instruct` | Apache-2.0 | No | Older, rock-solid, has an official first-party GGUF repo |
| `HuggingFaceTB/SmolLM2-135M-Instruct` / `-360M-Instruct` | Apache-2.0 | No | Smallest credible option; 135M Q4_K_M ≈ 105 MB |
| `TinyLlama/TinyLlama-1.1B-Chat-v1.0` | Apache-2.0 | No | Fine, but 2023-era output quality |
| `google/gemma-3-270m` | Gemma Terms | **YES — gated** | ⚠️ Sources conflict on Apache-2.0 vs Gemma Terms; **gated either way**. Avoid. |
| `meta-llama/Llama-3.2-1B` | Llama 3.2 Community | **YES — gated** | Avoid |

**Zero-friction demos require ungated models.** A gated model means every person who clones the repo needs an HF token and must accept terms — that alone disqualifies gemma-3-270m and Llama-3.2-1B.

### 5.2 GGUF quantizations (verified filenames and sizes)

**`unsloth/Qwen3-0.6B-GGUF`:**

| File | Size |
|---|---|
| `Qwen3-0.6B-Q4_K_M.gguf` | **397 MB ← recommended** |
| `Qwen3-0.6B-Q5_K_M.gguf` | 444 MB |
| `Qwen3-0.6B-Q8_0.gguf` | 639 MB |
| `Qwen3-0.6B-BF16.gguf` | 1.2 GB |
| `Qwen3-0.6B-UD-IQ1_S.gguf` | 215 MB (smallest) |

Also: `bartowski/Qwen_Qwen3-0.6B-GGUF` (imatrix quants). `Qwen/Qwen2.5-0.5B-Instruct-GGUF` is the **official first-party** GGUF repo and by far the highest-download 0.5B GGUF. First-party SmolLM GGUFs: `HuggingFaceTB/SmolLM2-360M-Instruct-GGUF`, `HuggingFaceTB/SmolLM2-1.7B-Instruct-GGUF`.

⚠️ `ggml-org/*-GGUF` (the llama.cpp team's own auto-converted mirror org, designed for `llama-server -hf`) carries mostly mid/large models; no `ggml-org/Qwen3-0.6B-GGUF` was found.

### 5.3 Memory and throughput budget

| Config | Resident RAM | Throughput |
|---|---|---|
| llama.cpp, Qwen3-0.6B Q4_K_M, 4k ctx | ~0.6–0.9 GB | ⚠️ unmeasured; expect tens of tok/s |
| llama.cpp, SmolLM2-135M Q4_K_M | ~0.2–0.3 GB | very fast |
| vLLM CPU, Qwen3-0.6B bf16, 8k ctx | ~2–4 GB (1.5 GB weights + 4 GiB default KV reservation) | ~12.5 tok/s ⚠️ single source |

With 18 GB total, minus Docker Desktop VM overhead and the kind node, budget **~10–12 GB usable**: comfortably 5–8 llama.cpp replicas, but only 2–3 vLLM CPU replicas. Another argument for llama.cpp as the default.

### 5.4 Apple Silicon / Docker Desktop gotchas

1. **No GPU/Metal inside the container, period.** Don't chase `--n-gpu-layers`.
2. **Never pull an amd64-only image** — it either fails with `no matching manifest` or silently runs under Rosetta at ~5–20× slowdown. This is the trap with SGLang (`-xeon` = x86) and TGI. Verify with `docker image inspect --format '{{.Architecture}}'`.
3. **Docker Desktop VM memory is a hard cap** set in Settings → Resources; the default is often 8 GB, not the host's 18. Raise it to ~12 GB and leave macOS headroom.
4. **AVX512/AVX2 optimizations in every vLLM blog post do not apply** — arm64 uses NEON. Expect lower numbers than published x86 benchmarks.
5. **BF16 depends on M1 (no) vs M2+ (yes)** ⚠️ and passthrough into the VM was not verified.

### 5.5 Shipping the model to the cluster

| Approach | Verdict |
|---|---|
| **Bake the GGUF into the engine image** | Simplest and fully offline, but couples model version to server version — bad when you want to canary a *model* change independently |
| **initContainer + `emptyDir`** | Good for exercising operator lifecycle logic, but re-downloads 397 MB per pod per restart; not air-gapped |
| **PVC (kind ships `local-path-provisioner`)** | RWO on a single node works, but sharing across replicas is awkward; best as a cache layer |
| **⭐ ModelCar / OCI sidecar (KServe pattern)** | **Best fit.** Decouples model from engine (separate canary axes), works offline via a local registry, uses only stable primitives |
| **ImageVolume (KEP-4639)** | **Broken on kind — see below** |

**ImageVolume status:** alpha in v1.31, [beta in v1.33](https://kubernetes.io/blog/2025/04/29/kubernetes-v1-33-image-volume-beta/), enabled by default in v1.35, ⚠️ GA in 1.36 per secondary sources only. Requires containerd ≥2.1.

🚨 **Blocker: [kind issue #4099](https://github.com/kubernetes-sigs/kind/issues/4099) is OPEN** (filed 2026-01-29). Images loaded via `kind load docker-image` are **not visible** to the image-volume mount path — `failed to mount image volume: image not found`. Only images containerd pulls from a registry work. No maintainer response as of this research. Workaround: run a local registry on the kind network and push there.

**OCI model artifact standards did land in 2026:** **CNCF ModelPack** was accepted into the CNCF Sandbox as *"the first vendor-neutral open standard for packaging ML artifacts as OCI objects"* ([CNCF, 2026-08-12](https://www.cncf.io/blog/2026/08/12/advancing-ai-model-interoperability-with-docker-and-modelpack/)). Docker Model Runner went GA Sept 2025 and `docker model package --format=cncf` emits ModelPack artifacts. Docker Hub's `ai/` namespace serves them live — e.g. `ai/smollm2:135m-q4_K_M` (105.5 MB), media type `application/vnd.cncf.model.manifest.v1+json`.

⚠️ These are **OCI artifacts, not runnable images** — a ModelCar *sidecar container* cannot run one, and whether they mount via k8s ImageVolume was not verified.

**Practical recommendation for kind:** a local registry on `localhost:5001` plus a small baked model image is both the fastest inner loop (`docker push` is layer-incremental; `kind load` re-copies the whole tarball) and the thing that would unblock ImageVolume later.

---

## 6. Autoscaling

### 6.1 KEDA has displaced prometheus-adapter

| Project | Latest | Assessment |
|---|---|---|
| `kubernetes-sigs/prometheus-adapter` | **v0.12.0, 2024-05-17** | **No release in ~28 months.** A 2+ year gap on a component that must track k8s API libs is a red flag. |
| `kedacore/keda` | **v2.20.2, 2026-07-31** | CNCF Graduated; quarterly minors, monthly patches |

**KEDA is the 2026 community default for scaling on a Prometheus metric.** It bundles its own metrics adapter (registering `external.metrics.k8s.io`), queries Prometheus directly, and supports scale-to-zero, which HPA alone cannot do.

Hard constraint: **only one APIService can serve `external.metrics.k8s.io` per cluster** — KEDA and prometheus-adapter-in-external-mode cannot coexist.

`ScaledObject` (`keda.sh/v1alpha1`) knobs that matter: `pollingInterval`, `cooldownPeriod`, `idleReplicaCount` (scale-to-zero), `fallback.{failureThreshold,replicas}` (behavior when Prometheus is unreachable), `advanced.horizontalPodAutoscalerConfig.behavior`, and on the Prometheus trigger `threshold`, `activationThreshold`, and **`ignoreNullValues`**.

**`ignoreNullValues: "false"` is the setting most demos get wrong** — with the default `true`, an empty Prometheus result is treated as `0` and silently scales you down.

### 6.2 What to scale on — the 2026 consensus

**Google GKE's [inference autoscaling best practices](https://docs.cloud.google.com/kubernetes-engine/docs/best-practices/machine-learning/inference/autoscaling)** (primary source) recommends two server-level metrics:

- **Queue size** — use *"when optimizing throughput and cost."* **Start with a target between 3 and 5** and increase until latency is acceptable.
- **Batch size** — for latency-sensitive workloads where queue-based scaling isn't fast enough.

And explicitly warns against:
- **GPU utilization**: *"does not measure how much work is being done while the GPU is active."*
- **CPU and memory utilization** as sole indicators.

GKE's TPU guidance adds: use `num_requests_waiting` for throughput scaling and cache utilization for latency-sensitive cases. AWS EKS docs use an example threshold of 25 waiting requests per pod. llm-d's Workload Variant Autoscaler monitors *"KV cache utilization and queue depth"* plus latency SLOs.

**Gateway API Inference Extension**'s Endpoint Picker tracks *"KV-cache utilization, queue length of pending requests, and active LoRA adapters"*, with a documented default **KV-cache saturation threshold of 0.8**. Notably, **HPA-on-aggregate-metrics is roadmap, not shipped** — a real gap in the ecosystem.

**Synthesized control law:**

```
Primary signal : queue depth per pod, target 3–5
                 llamacpp:requests_deferred  |  vllm:num_requests_waiting
Saturation gate: scale up above 0.8
                 vllm:kv_cache_usage_perc    |  (llama.cpp: busy slots / --parallel)
SLO gate       : p95 TTFT — for CANARY ANALYSIS and ROLLBACK, not for scaling
Never          : CPU, memory, or GPU utilization
```

**Scale on queue, gate on latency.** That split is the 2026 consensus and is directly citable.

⚠️ **`llamacpp:requests_deferred` semantics are not documented in detail.** It is a gauge of requests deferred because no slot is free, so with `--parallel N` it only becomes non-zero once all N slots are busy. This must be verified empirically before being wired to an autoscaler; if it stays at 0 under load, fall back to `requests_processing / --parallel` as a utilization ratio, or scrape `/slots`.

### 6.3 Operator-owned loop vs delegating to HPA/KEDA

| Approach | Pros | Cons |
|---|---|---|
| **Operator emits an HPA/ScaledObject** | Idiomatic; gets stabilization windows, `behavior` policies, and KEDA's fallback for free; instantly recognizable | Adds a runtime dependency |
| **Operator runs its own loop** | Can implement LLM-aware logic HPA can't ("don't scale down mid-canary"); demonstrates control-theory understanding | Reimplements flapping suppression, stabilization, rate limiting; two writers of `.spec.replicas` fight |

If implementing a loop, reuse the upstream HPA algorithm faithfully: `desired = ceil(current × metric/target)`, a **tolerance band** (HPA's default is 0.1 — without it, queue depth 4.1 against target 4 causes a scale event every poll), scale-up stabilization 0s, scale-down 300s, and a hard "no scaling while a rollout or canary is in flight" gate.

**Note there is no meaningful "HPA mode".** An external HPA writes `.spec.replicas` through `/scale` and the controller reconciles it like any other spec change — an HPA enum value would change zero lines of behavior. Implementing `/scale` correctly and testing that a stock HPA drives the CR is the substantive version of that feature.

### 6.4 Scale-to-zero and cold starts

- **KEDA `idleReplicaCount: 0`** works for any trigger but drops HTTP requests while at zero.
- **KEDA HTTP Add-on** — status **beta (v0.15 docs, v1.0 planned, not released)** as of 2026-09. Its interceptor *holds* the request while scaling 0→1, so nothing is dropped.
- **Knative Serving** — most mature request-buffering scale-to-zero, but its footprint (net-* controller, activator, autoscaler, webhook, per-pod queue-proxy) is heavy for kind and it takes over the Deployment shape.
- ⚠️ **LLM-specific cold-start trap:** aggressive liveness/readiness probes plus a long model load causes thrashing, and **health checks themselves generate traffic that defeats scale-to-zero** if routed through the interceptor. Fix: `startupProbe` with a generous `failureThreshold`, and exclude probe paths from the scaler's route.
- For a tiny CPU GGUF model, cold start is ~2–10 s — which makes a *live* scale-from-zero demo genuinely feasible, something that's hard to show with a real GPU model.

### 6.5 In-place vertical scaling

KEP-1287: alpha in v1.27, [beta in v1.33](https://kubernetes.io/blog/2025/05/16/kubernetes-v1-33-in-place-pod-resize-beta/), **GA (container-level) in v1.35**. Pod-level resources went beta in 1.34, and in-place resize of pod-level resources went beta in [v1.36](https://kubernetes.io/blog/2026/04/30/kubernetes-v1-36-inplace-pod-level-resources-beta/). VPA's `InPlaceOrRecreate` mode is beta.

On a 1.35 kind cluster this is GA and on by default: `kubectl patch pod ... --subresource resize` with `resizePolicy: [{resourceName: cpu, restartPolicy: NotRequired}]` gives a llama.cpp pod more CPU without a restart. A compelling 20-second demo and a natural `spec.verticalScaling` stretch goal.

---

## 7. Observability

### 7.1 Prometheus stack on kind

**kube-prometheus-stack chart 88.6.2 / appVersion v0.93.1** (✅ re-verified from `Chart.yaml` on `main`):

| Component | Version |
|---|---|
| Prometheus | `quay.io/prometheus/prometheus:v3.14.0-distroless` |
| Alertmanager | `quay.io/prometheus/alertmanager:v0.34.0` |
| Thanos sidecar | `quay.io/thanos/thanos:v0.42.4` |
| grafana subchart | 12.11.2 |
| kube-state-metrics | 8.4.1 |
| prometheus-node-exporter | 4.56.3 |

⚠️ **Ecosystem change:** the Grafana Helm chart **moved from `grafana/helm-charts` to `grafana-community/helm-charts`** (authoritative after 2026-01-30). Any tutorial saying `helm repo add grafana https://grafana.github.io/helm-charts` is stale.

⚠️ Also: kube-prometheus-stack 88.6.2 pins grafana subchart 12.11.2 while the standalone chart is at 13.0.1/appVersion 13.2.0 — so it is one chart-major behind. Don't promise "Grafana 13" without checking or overriding `grafana.image.tag`.

**Slimming for a laptop.** Default install is ~1.5–2.5 GiB RSS and generates noisy failing targets, because kind's control-plane components bind to `127.0.0.1` and aren't scrapeable. Disable `kubeControllerManager`, `kubeScheduler`, `kubeProxy`, `kubeEtcd`, and Windows monitoring (keep `kubeApiServer` — that one works); cap resources; `retention: 6h`; `storageSpec: {}` (emptyDir, no PVC churn); trim `defaultRules`. Realistic slimmed footprint: **~700–900 MiB RSS, ~0.4 CPU idle**.

🚨 **The single most important values setting for an operator project:**

```yaml
prometheus:
  prometheusSpec:
    serviceMonitorSelectorNilUsesHelmValues: false
    podMonitorSelectorNilUsesHelmValues: false
    ruleSelectorNilUsesHelmValues: false
    probeSelectorNilUsesHelmValues: false
    scrapeConfigSelectorNilUsesHelmValues: false
```

Without these, Prometheus only picks up ServiceMonitors carrying `release: <helm-release>`, and **every ServiceMonitor the operator creates is silently ignored**. (The arguably more production-grade alternative: have the operator stamp a configurable `release:` label onto everything it creates, since real clusters do use selector labels.)

Also set `grafana.sidecar.dashboards.searchNamespace: ALL` so operator-created dashboard ConfigMaps in app namespaces are picked up.

**Alternatives considered:** prometheus-operator alone (~250 MiB, but you hand-write Grafana + datasources) · `prometheus-community/prometheus` (❌ no ServiceMonitor CRD, so there's nothing for the operator to integrate with) · Grafana Alloy (consumes ServiceMonitor/PodMonitor/ScrapeConfig but installs no CRDs and stores no data — a collector, not a backend) · **VictoriaMetrics `victoria-metrics-k8s-stack`** (genuinely 30–50% lighter, operator v0.74.1, own CRD set plus auto-conversion from `monitoring.coreos.com` — the best pure-RAM answer, but reviewers won't recognize the CRDs and every Flagger/KEDA/Pyrra example assumes Prometheus).

### 7.2 ServiceMonitor vs ScrapeConfig

prometheus-operator v0.93.1 CRD versions (read from `example/prometheus-operator-crd-full/`):

| Kind | Version |
|---|---|
| `Prometheus`, `Alertmanager`, `ThanosRuler`, `ServiceMonitor`, `PodMonitor`, `Probe`, `PrometheusRule` | **v1 (stable)** |
| `PrometheusAgent`, `ScrapeConfig` | **v1alpha1 only** |
| `AlertmanagerConfig` | v1alpha1 + v1beta1 |

**Use `ServiceMonitor` (v1).** ScrapeConfig exists for targets the operator can't discover via Services (static, EC2/Consul/HTTP SD); a Service-backed workload is exactly the ServiceMonitor case, and it's the only one of the two on a stable API. ScrapeConfig's [beta graduation](https://prometheus-operator.dev/docs/proposals/accepted/scrapeconfig-graduation/) is accepted-but-not-shipped, and upstream still says v1alpha1 CRDs are *"unstable, changes happen frequently, avoid in mission-critical environments."*

### 7.3 Depending on another operator's CRDs — the correct pattern

🚨 A well-known controller-runtime footgun: **if you watch a type whose CRD isn't installed, the manager's RESTMapper returns `meta.NoKindMatchError` and the manager crashes at `Start()`, and it does not self-heal when the CRD is later installed** — the RESTMapper is built once. ([controller-runtime #2589](https://github.com/kubernetes-sigs/controller-runtime/issues/2589), [#2424](https://github.com/kubernetes-sigs/controller-runtime/issues/2424), [#321](https://github.com/kubernetes-sigs/controller-runtime/issues/321))

Three-part pattern:

1. **Discovery probe at startup** via `discovery.ServerResourcesForGroupVersion` — cheap, no watch. Register the scheme unconditionally (harmless), but only enable the dependent code path when the probe succeeds. Log loudly when disabled.
2. **Defend at the write site anyway** — a CRD can be uninstalled while you run. On `meta.IsNoMatchError`, set a status condition (e.g. `MetricsRegistered=False, Reason=PrometheusOperatorCRDsAbsent`) and **do not requeue with error**.
3. **Add `Owns()` conditionally** — build the controller builder conditionally based on the probe result.

A common real-world variant (cert-manager, Tekton): also watch `CustomResourceDefinition` objects and, on seeing the CRD appear, either lazily `mgr.Add()` a controller or deliberately `exit(0)` and let the Deployment restart. **The exit(0) approach is what most production operators actually do** and is far easier to reason about.

**Ship the dependent RBAC unconditionally** — granting on a nonexistent API group is legal, and omitting it produces a confusing forbidden error at exactly the wrong moment.

### 7.4 What the operator's workloads should expose

The engine-metric gap from §4.1 forces an architectural decision. Options for getting RED + TTFT metrics when the engine has no histograms:

| Option | Assessment |
|---|---|
| **(a) A small Go reverse-proxy sidecar** emitting `*_requests_total{code}`, `*_request_duration_seconds_bucket`, `*_ttft_seconds_bucket` (measured at the first SSE chunk), `*_output_tokens_total` | **Best answer.** Gives real RED metrics and real TTFT, is the piece that makes canary analysis possible at all, and makes analysis **engine-independent** — the same metric names mean the same thing under llama.cpp, vLLM, or a stub |
| (b) Metrics from a Gateway/Envoy data plane | Gives status codes and latency but **not TTFT** |
| (c) Metrics from the load generator | Weak — not a server-side SLI |

**Three implementation requirements or the metrics are silently wrong:**

```go
rp := &httputil.ReverseProxy{
    FlushInterval: -1,  // MANDATORY: flush immediately, never buffer.
                        // Without this, Go buffers the SSE stream and
                        // every TTFT == total duration.
    Transport: &http.Transport{
        DisableCompression: true,  // gzip on a stream => buffering => wrong TTFT
    },
    // ModifyResponse: wrap Body to detect the first content-bearing frame
}
```

1. `FlushInterval: -1` **and** `DisableCompression: true`.
2. TTFT measured at the **first non-empty SSE `data:` frame** (a `choices[0].delta.content` with content), not the first response byte — servers send headers immediately.
3. **Never record TTFT for non-streaming requests** — `TTFT == duration` poisons the histogram. Load generators must send `"stream": true`.

### 7.5 The operator's own metrics

Built-in controller-runtime metrics ([reference](https://book.kubebuilder.io/reference/metrics-reference)):

```
controller_runtime_reconcile_total{controller,result}   # success|error|requeue|requeue_after
controller_runtime_reconcile_errors_total{controller}
controller_runtime_terminal_reconcile_errors_total{controller}
controller_runtime_reconcile_time_seconds{controller}   # histogram
controller_runtime_reconcile_panics_total{controller}
controller_runtime_active_workers{controller}
controller_runtime_max_concurrent_reconciles{controller}
workqueue_depth / _adds_total / _queue_duration_seconds / _work_duration_seconds
  / _unfinished_work_seconds / _longest_running_processor_seconds / _retries_total
rest_client_requests_total{code,method,host} / _request_duration_seconds
leader_election_master_status{name}
```

⚠️ Known gotcha: [k/k#128326](https://github.com/kubernetes/kubernetes/issues/128326) — `workqueue_*` names collide between controller-runtime's and client-go's registrations; [kueue#7222](https://github.com/kubernetes-sigs/kueue/issues/7222) reports `workqueue_depth` silently missing in some setups. `curl` your own `/metrics` before building a dashboard panel on them.

Register custom metrics on `sigs.k8s.io/controller-runtime/pkg/metrics`.`Registry` — never `promauto`'s default registry.

**Secure serving** replaced kube-rbac-proxy: `metricsserver.Options{BindAddress: ":8443", SecureServing: true, FilterProvider: filters.WithAuthenticationAndAuthorization}`. The scraping ServiceAccount needs `create` on `tokenreviews` (authentication.k8s.io) and `subjectaccessreviews` (authorization.k8s.io), plus `get` on the `/metrics` nonResourceURL. Without a cert-manager cert, controller-runtime self-signs and the ServiceMonitor needs `tlsConfig.insecureSkipVerify`.

### 7.6 Metric naming and OTel gen_ai semconv

Follow [Prometheus naming conventions](https://prometheus.io/docs/practices/naming/): `snake_case`, a single-word app prefix, base units (`seconds`, `bytes`, `ratio` — never `_ms`, never `_percent`), `_total` on counters, and no label-cardinality explosion (never a request ID or prompt in a label).

**On OTel gen_ai semconv (verified 2026-09-01):** it **moved out of `open-telemetry/semantic-conventions` into its own repo, `open-telemetry/semantic-conventions-genai`**. Every gen_ai metric, attribute, and span still carries the **`Development`** badge — the current term for experimental. **Nothing in gen_ai is Stable in 2026.**

Server-side inference metrics defined (all Histogram, all Development):

| Metric | Unit | Note |
|---|---|---|
| `gen_ai.server.request.duration` | s | time-to-last-byte / last output token |
| `gen_ai.server.time_to_first_token` | s | TTFT — includes queue + prefill |
| `gen_ai.server.time_per_output_token` | s | TPOT / inter-token latency, excludes first token |
| `gen_ai.client.token.usage` | {token} | attr `gen_ai.token.type` ∈ input/output |

Advised TTFT buckets: `0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5, 0.75, 1.0, 2.5, 5.0, 7.5, 10.0`.

**Guidance: don't build canary gates on an unstable spec.** Emit your own prefixed metrics as the contract, and optionally emit gen_ai-conformant names behind a flag, documented as Development-stage.

Related interoperability wart worth knowing: **vLLM emits OTel traces** (`--otlp-traces-endpoint`) with attributes `gen_ai.latency.time_to_first_token` and `gen_ai.latency.time_in_queue` — **vLLM's own names, not the semconv names** (`gen_ai.server.time_to_first_token`).

### 7.7 Grafana dashboards as code

| Approach | 2026 status | Verdict |
|---|---|---|
| **JSON in ConfigMap + Grafana sidecar** (`grafana_dashboard: "1"`, `folderAnnotation: grafana_folder`, `searchNamespace: ALL`) | Built into the kube-prometheus-stack grafana subchart; hot-reloads without a pod restart | ✅ **The natural fit for an operator**: CR → owned ConfigMap → dashboard appears. Zero extra dependency. |
| **grafana-operator v5** (v5.25.0, `grafana.integreatly.org/v1beta1`) | Actively developed, official Grafana project | Good, but it's a *second* operator with its own `instanceSelector` semantics, duplicating the Grafana kube-prometheus-stack already installs. The multi-cluster / Grafana Cloud answer. |
| **Grafana Foundation SDK (Go)** (`github.com/grafana/grafana-foundation-sdk/go`, published 2026-06-12; Dashboard V2 `dashboard.grafana.app/v2` stable in Grafana 13+) | Actively maintained; ⚠️ no explicit `v1.0.0` Go module tag or GA announcement found | Highest-signal for a Go project — typed Go dashboards, `go generate` → JSON, `//go:embed` into a ConfigMap. But the API churns; betting several dashboards on it is a bad trade. |
| **Grafonnet / jsonnet** | Maintained; [issue #264](https://github.com/grafana/grafonnet/issues/264) shows Dashboard V2 support lags the Foundation SDK | Skip — a second language in a Go project |

**Existing dashboards to adapt** (verified grafana.com IDs): **23991** (vLLM) · **25502** (LLM Inference Dashboard, SGLang/vLLM unified) · **25043** (vLLM Dashboard) · **25263** (vLLM Metrics, used in the EKS AI/ML guide) · **24756** (vLLM Monitoring V2) · **25494** (vLLM Load Analysis). vLLM's own `examples/online_serving/prometheus_grafana/grafana.json` is the best structural reference. **No official llama.cpp dashboard exists** — only community gists.

### 7.8 SLOs and alerting

**SLIs that make sense for LLM inference:**

| SLI | Definition | Suggested SLO |
|---|---|---|
| Availability | non-5xx / total | 99.5% |
| **TTFT latency** | fraction of requests with TTFT ≤ 1 s | 95% |
| E2E latency | fraction with total duration ≤ 10 s | 95% |
| Queue wait | fraction never deferred | 99% |

**The LLM-specific nuance worth writing down: e2e latency is a bad primary SLI for LLMs**, because it scales with output length. TTFT and TPOT are length-independent and are the right SLIs — which is exactly why OTel's gen_ai semconv defines them separately.

**Multi-window multi-burn-rate ladder** (Google SRE Workbook), for a 99.5% SLO (error budget 0.005):

| Severity | Burn rate | Long window | Short window | Budget consumed |
|---|---|---|---|---|
| page | 14.4 | 1h | 5m | 2% in 1h |
| page | 6 | 6h | 30m | 5% in 6h |
| ticket | 3 | 1d | 2h | 10% in 1d |
| ticket | 1 | 3d | 6h | 10% in 3d |

Write recording rules for the SLI ratio at each window first, then the four alerts on top.

**Generators:** **Sloth** (`sloth.dev`) is CLI-first — `sloth generate` produces checked-in YAML with zero runtime footprint. **Pyrra** watches `ServiceLevelObjective` CRs, generates `PrometheusRule` objects, and ships an error-budget burndown UI. For a laptop demo Sloth is better; hand-writing the burn-rate rules for the primary SLO (~40 lines) demonstrates understanding rather than outsourcing it.

⚠️ **Practical caveat for a demo:** the 1d and 3d windows will show "No Data" in a 10-minute demo, which looks worse than not having them. Ship the 5m/1h and 30m/6h tiers live and gate the long tiers behind a values toggle, with an explicit comment that the demo SLO window is compressed.

### 7.9 Tracing and logs (optional)

- **vLLM emits OTel traces** natively (`--otlp-traces-endpoint`, `pip install vllm[otel]`).
- ⚠️ **llama.cpp has no OTel tracing** — its observability is `/metrics` + `/slots` only. Traces would have to come from a sidecar.
- **Lightest laptop stack:** Grafana Alloy → Tempo (monolithic, `filesystem` backend, ~100 MiB) + Loki (SingleBinary, `filesystem`, ~150 MiB). Do not add Mimir. Total ~300 MiB.
- **Verdict: stretch goal at most.** Metrics + autoscaling + canary + SLOs is already a coherent project; adding LGTM risks turning it into a demo of Helm installs.

---

## 8. Progressive delivery — prior art and vocabulary

### 8.1 Project status (✅ re-verified)

| Project | Latest | Health |
|---|---|---|
| **Argo Rollouts** | **v1.10.0**, 2026-08-27 | Very active. v1.9.1 (2026-07-17) fixed CVE-2026-35469 |
| **Flagger** | **v1.45.0**, 2026-09-01 | **Actively maintained** — this corrects a widespread assumption that Flagger was winding down. Cadence did slow in 2025 (v1.41 Apr 2025 → v1.42 Oct 2025) then recovered (v1.43 Apr, v1.44 Jul, v1.45 Sep 2026) |
| **OpenKruise** | v1.9.1, 2026-07-04 | Active, but progressive delivery lives in the separate OpenKruise Rollouts subproject |

⚠️ Flagger caveat: since v1.39.0 (Nov 2024), Gateway API v1alpha2 is deprecated in Flagger and slated for removal. Use v1/v1beta1 route types.

### 8.2 Argo Rollouts vocabulary

**Steps** (`spec.strategy.canary.steps`, an ordered list of single-key objects):

- **`setWeight: <0-100>`** — traffic percentage to canary. **Without a traffic router, the controller approximates the weight by proportional replica scaling** — which is exactly the kind-friendly, zero-dependency path.
- **`pause: {}`** — indefinite, requires manual promotion. **`pause: {duration: 30s}`** — timed.
- **`setCanaryScale:`** — decouples replica count from traffic weight (`replicas` | `weight` | `matchTrafficWeight`), enabling shadowing and pre-warming.
- **`analysis:`** — inline analysis that *blocks* the rollout. **`experiment:`** — ephemeral A/B pods.

**Analysis** (`AnalysisTemplate` → `AnalysisRun`), terminating as **`Successful | Failed | Inconclusive`**. Per-metric fields: `interval`, `count` (0 = background, runs until the rollout completes), `initialDelay`, `successCondition` (e.g. `result[0] >= 0.95`), `failureCondition`, `failureLimit` (default 0 = zero tolerance), `consecutiveSuccessLimit`, `consecutiveErrorLimit`, `inconclusiveLimit`.

**Rollback:** a failed analysis aborts the Rollout, sets canary weight to 0, and marks it `Degraded`. **`Inconclusive` pauses and requires human intervention** — a genuinely good third state most homegrown operators omit.

### 8.3 Flagger vocabulary

`flagger.app/v1beta1`, kinds `Canary` / `MetricTemplate` / `AlertProvider`. Built-in metrics: `request-success-rate` (% non-5xx) and `request-duration` (P99, **milliseconds**).

| Field | Meaning |
|---|---|
| `interval` | canary loop schedule |
| `threshold` | **max failed checks before rollback** |
| `maxWeight` / `stepWeight` / `stepWeights[]` | weight ladder (linear or explicit, e.g. `[1,2,10,80]`) |
| `stepWeightPromotion` | increment during promotion |
| `iterations` | check count for A/B and Blue/Green |
| `mirror` / `mirrorWeight` | shadow traffic |
| `match` | A/B header/cookie conditions |
| `metrics[]` | `name`, `interval`, `thresholdRange{min,max}`, `templateRef` |
| `webhooks[]` | `confirm-rollout`, `pre-rollout`, `rollout`, `confirm-promotion`, `post-rollout`, `rollback`, `event` |
| `primaryReadyThreshold` / `canaryReadyThreshold` | % pods that must be Ready |

FSM: `Initializing → Initialized → Progressing → Waiting → Promoting → Finalising → Succeeded | Failed`.

**Which vocabulary to prefer: Flagger's**, for two reasons. Its `thresholdRange{min,max}` + failure-budget `threshold` + `MetricTemplate` indirection is smaller and more declarative than Argo's expression-language `successCondition`, and it maps cleanly onto a compact CRD stanza. And Flagger is **fully automated by default** (metric-gated promotion) whereas Argo Rollouts requires manual promotion unless every pause is timed — automation is what a control plane should demonstrate. Flagger's controller is also lighter (~24 MiB / ~18 mcpu vs ~35 MiB with spikes to ~168 mcpu).

**But take Argo's three-valued verdict.** In fact, four values are better, because they separate two failure modes Flagger conflates:

| Verdict | Meaning | Consequence |
|---|---|---|
| `Fail` | Query OK, value outside threshold | counts toward the failure budget ⇒ rollback |
| `Error` | Provider unreachable / timeout / bad query | counts toward `consecutiveErrorLimit` only |
| `Inconclusive` | Query OK but unusable: no samples, NaN, below min traffic | counts toward `inconclusiveLimit` |
| `Pass` | In range | resets Error and Inconclusive counters |

**A monitoring outage must never trigger a rollback.** Collapsing `Error` into `Fail` is how homegrown canary controllers roll back for the wrong reason.

### 8.4 PromQL for canary analysis, and two traps

Assuming shim-emitted metrics with a `variant ∈ {primary, canary}` label:

```promql
# success rate (%)
100 * (
  sum(rate(inference_requests_total{...,variant="$v",code!~"5.."}[1m]))
  / clamp_min(sum(rate(inference_requests_total{...,variant="$v"}[1m])), 1e-9)
)

# p95 end-to-end latency
histogram_quantile(0.95,
  sum by (le) (rate(inference_request_duration_seconds_bucket{...,variant="$v"}[1m])))

# p95 TIME TO FIRST TOKEN — the LLM-specific one
histogram_quantile(0.95,
  sum by (le) (rate(inference_ttft_seconds_bucket{...,variant="$v"}[1m])))

# output tokens/sec — available from llama.cpp natively
sum(rate(llamacpp_tokens_predicted_total{...,variant="$v"}[1m]))

# TPOT (inter-token latency, s/token) — natively
  sum(rate(llamacpp_tokens_predicted_seconds_total{...,variant="$v"}[1m]))
/ clamp_min(sum(rate(llamacpp_tokens_predicted_total{...,variant="$v"}[1m])), 1e-9)

# queue depth — the autoscaling signal
sum(llamacpp_requests_deferred{...,variant="$v"})
```

🚨 **Trap 1 — no traffic yields NaN.** A success-rate expression with a zero denominator returns no samples, and a naive `value < 99 → fail` treats "no data" as either failure or success depending on how it's written. `clamp_min(..., 1e-9)` keeps the expression evaluable, but the real fix is a **minimum-traffic precondition as a first-class spec field**, not a PromQL idiom buried in a template. Without it a canary "passes" against zero traffic and the analysis is theater. (KEDA's `ignoreNullValues` is the analogous knob on the autoscaling side.)

🚨 **Trap 2 — few canary pods yield noisy quantiles.** Use `interval` ≥ 1m and require ≥ 3 consecutive failures.

### 8.5 Traffic splitting on kind without a mesh

| Option | Verdict |
|---|---|
| **Replica-ratio weighting** (two Deployments, one Service) | Works, zero dependencies, honest L4 approach. Weight granularity is `canaryReplicas/total`, so 10% needs 9+1. Good enough for a first milestone; document the quantization. |
| **Flagger `meshProvider: kubernetes`** | ✅ **Blue/Green only.** Flagger explicitly: for Blue/Green no mesh or ingress controller is required. **Weighted canary needs an L7 provider.** |
| **Gateway API `HTTPRoute` `backendRefs[].weight`** | True L7 weighting, core conformance, stable API. The correct production answer. |
| Istio | ~5 components plus sidecars/ztunnel. Overkill at 18 GB, though ambient is lighter. |
| Linkerd | Lightest mesh (~2 core pods + micro-proxies). Viable if mTLS is wanted in the story. |
| **ingress-nginx** | ❌ **Retired.** No releases or patches after March 2026; repos read-only. Its designated successor **InGate is also retired**. |

**Gateway implementations that run cleanly on kind (2026):** Envoy Gateway v1.9.1 (recommended; one-line OCI Helm install; also a Gateway API Inference Extension implementation) · kgateway (ex-Gloo, CNCF, the most-recommended ingress-nginx successor) · NGINX Gateway Fabric (official NGINX successor, has a documented Inference Extension integration) · Contour, Traefik, Cilium, Istio.

🚨 **The trap that will silently wreck a replica-weighted canary demo: kube-proxy load-balances per *connection*, not per *request*.** With HTTP/1.1 keep-alive — which every load generator and every OpenAI SDK uses by default — N concurrent clients open N long-lived connections, each pinned to one pod for the whole run. At a 20% requested weight the realized split is an arbitrary multiple of 1/N, **stable and wrong for the entire canary window**. The PromQL then compares a canary that received ~0% of traffic against a primary that received 100%, and the canary looks perfectly healthy right up until promotion. This is a silent correctness bug, not a performance issue.

Mitigations in increasing order of goodness: (1) load generator uses `--disable-keepalive`; (2) a small per-request weighted router proxy (~150 lines on top of a shim that already exists); (3) Gateway API `HTTPRoute` weights — correct and production-shaped, but ~300 MB and 2 extra pods on an already tight RAM budget.

### 8.6 Gateway API and the Inference Extension

**Gateway API v1.6.1** (2026-07-16). v1.6.0 graduated UDPRoute and TCPRoute to v1/GA and moved the CORS filter to the standard channel. ⚠️ Breaking in rc.2: TLS must now be configured for HTTPS listeners. The relevant primitive — `HTTPRoute.spec.rules[].backendRefs[].weight` — is stable.

**Gateway API Inference Extension v1.6.0** (2026-08-17), stable group `inference.networking.k8s.io/v1`, `InferencePool` v1 CRD.

⚠️ **Major v1.6.0 restructuring:** the full-featured **Endpoint Picker (EPP)**, Body-Based Routing, and Latency Predictor all **moved out to the llm-d repository**. `InferenceObjective`, `InferenceModelRewrite`, and `EndpointPickerConfig` were **removed** upstream, leaving the core CRD/spec plus a lightweight reference EPP for conformance. `spec.endpointPickerRef` is now optional. **Docs and tutorials written before 2026-08 point at APIs that no longer exist upstream.**

What it provides: KV-cache-aware and LoRA-adapter-aware routing — the EPP picks a specific model-server pod per request instead of round-robin, cutting TTFT and improving utilization across multi-replica vLLM.

**Usable on kind?** Partially. The CRDs install trivially and Gateway API + Envoy Gateway gives working weighted routing. But the EPP's value proposition — KV-cache-aware routing — is **meaningless on a tiny CPU model**, since there's no GPU memory pressure to optimize. Worth referencing as the production evolution path; not worth a hard dependency.

### 8.7 Prior art to differentiate from

| Project | What it is | Relationship |
|---|---|---|
| **KubeAI** ([kubeai.org](https://www.kubeai.org/)) | Lightweight operator; **`Model` CRD**; OpenAI-compatible proxy; **scale-from-zero with request queueing during scale-up**; engines vLLM/Ollama/FasterWhisper/Infinity. **No Prometheus-adapter, Knative, or Istio dependency.** Ships CPU-only quickstarts (`qwen2-500m-cpu`). | **Closest prior art.** Borrow: the Model CRD shape, proxy-based autoscaling that avoids the custom-metrics stack, CPU-only quickstart profiles. **Differentiate on canary + automated rollback, which KubeAI does not do.** |
| **KServe v0.17** | `InferenceService` / `LLMInferenceService` CRD, GenAI-first, built on llm-d; **modelcar** OCI sidecar storage; Knative-based canary | Borrow the canary semantics and the modelcar pattern; differentiate by being CRD-native without Knative/Istio |
| **llm-d** | CNCF Sandbox since 2026-03-24. Disaggregated prefill/decode, KV-cache-aware scheduling, `InferencePool` + `InferenceObjective`. Workload Variant Autoscaler = queue depth + cache utilization + latency SLOs + scale-to-zero | GPU-scale and multi-node — out of scope, but **the design north star for the autoscaling signal choice** |
| **AIBrix** (ByteDance) | SLO-aware second-level LLM autoscaling on KV-cache and inference-aware metrics; LoRA management; distributed KV cache ([arXiv:2504.03648](https://arxiv.org/pdf/2504.03648)) | Reference architecture to cite |
| **vLLM production-stack** | vLLM + KV-cache-aware router + bundled Prometheus/Grafana | Borrow the Grafana dashboard JSON |
| **Ramalama** (Red Hat) | Runs models in OCI containers, auto-selects llama.cpp / vLLM per model | Interesting for the model-as-OCI-artifact story; not a controller |
| **LLMKube** (`defilantech/llmkube`) | ⚠️ A third-party "Kubernetes operator for self-hosted LLMs" surfaced but **not verified in depth** | **Worth reviewing before starting — may overlap substantially** |

**The differentiating thesis:** a generic progressive-delivery controller (Argo Rollouts, Flagger) is generic over workloads; a *model-aware* control plane wants LLM-specific health signals — TTFT, tokens/sec, queue depth, KV-cache pressure, model-load readiness — that a generic canary controller can't express. That is exactly the premise the whole inference-gateway ecosystem is built on, and it justifies building rather than adopting.

---

## 9. Development, testing, and release engineering

### 9.1 Local dev loop

**kind + local registry.** `kind load docker-image` copies the whole image tarball into every node's containerd on every change — slow for a multi-GB model image, and it requires `imagePullPolicy: IfNotPresent`/`Never` (a `:latest` tag with the default `Always` will try Docker Hub and fail). The [documented local-registry approach](https://kind.sigs.k8s.io/docs/user/local-registry/) is layer-deduplicated and materially faster. For kind v0.27.0+ it uses containerd's `config_path` mechanism with a per-node `hosts.toml`, the registry container joined to the `kind` docker network, and a KEP-1755 `local-registry-hosting` ConfigMap in `kube-public`. ⚠️ kind v0.32.0 changed containerd config to v4 format — re-check any script written for ≤0.31.

**Inner loop.** `make run` — running the manager **on the host against the kind kubeconfig**, with only CRDs installed — is the real inner loop: no image build, sub-second iteration, full debugger. Reserve `make docker-build deploy` for in-cluster concerns (RBAC correctness, leader election, webhook certs, metrics auth). Tilt (v0.37.5, live-update + web UI, Starlark config) and Skaffold (pipeline-shaped, YAML, shareable with CI) are both actively maintained with **no industry consensus** between them; adding an optional `Tiltfile` is a nice touch, but a reliable `make deploy` is the stronger production signal.

**metrics-server on kind** needs `--kubelet-insecure-tls`, because kind nodes use self-signed kubelet serving certs not signed by the cluster CA, so all `PodMetrics` come back empty. Append the arg (`/spec/template/spec/containers/0/args/-`) rather than prepending as several blog posts do. ⚠️ [metrics-server#1695](https://github.com/kubernetes-sigs/metrics-server/issues/1695) suggests some setups also need Service-level changes.

**cert-manager on kind** (v1.21.1): install, then **wait for the webhook to be ready** before applying an Issuer — applying too early is the #1 kind flake. Use a self-signed `ClusterIssuer`. ⚠️ v1.21.0 has three breaking changes (removed default `tokenrequest` RBAC, restricted Challenge/Order permissions, removed metrics Helm values).

### 9.2 Testing

**envtest binaries moved.** They are no longer at the old GCS bucket; post-1.29.3 binaries are published as **releases on the `controller-tools` repo**, indexed by [`envtest-releases.yaml`](https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/master/envtest-releases.yaml). Verified live: `envtest-v1.37.0` (2026-08-27), `envtest-v1.36.2` (2026-06-26), multi-platform **including darwin/arm64**. `setup-envtest` still lives in controller-runtime (`sigs.k8s.io/controller-runtime/tools/setup-envtest`, need ≥ v0.19.0 for the new location). Kubebuilder v4.15.0 pins the ENVTEST version in generated projects — a reproducibility win; don't unpin it.

On macOS arm64 envtest runs `etcd` + `kube-apiserver` natively on the host, ~5 s startup, no Docker required. That makes it the primary test tier.

**Ginkgo v2 vs plain Go.** Kubebuilder still scaffolds Ginkgo v2 + Gomega. There is a long-running community push ([kubebuilder#2171](https://github.com/kubernetes-sigs/kubebuilder/issues/2171)) toward plain Go table tests, but as of v4.15.0 the default hasn't changed. The pragmatic 2026 consensus is hybrid: **plain Go table tests for pure functions** (state machines, weight computation, verdict evaluation — where the bulk of the logic should live), Ginkgo + envtest for reconcile integration, Chainsaw for e2e.

**Fake client caveats — do not build a test strategy on it.** `pkg/client/fake` is not an apiserver: no defaulting, no OpenAPI/CEL validation, no admission, no real garbage collection of owner-referenced objects, and status-subresource semantics require `WithStatusSubresource(...)` or writes behave wrongly. **SSA support was historically absent** ([#2341](https://github.com/kubernetes-sigs/controller-runtime/issues/2341)) and v0.23/v0.24 contain multiple SSA fixes — meaning it's a moving target. Prefer envtest.

**E2E: Chainsaw.** Kyverno Chainsaw v0.2.15 (2026-05-06), 87 tagged releases, actively maintained — declarative YAML, partial/subset assertions, `($x)` bindings, JMESPath, built-in polling with timeouts, and `kuttl`→`chainsaw` auto-migration. **KUTTL is stagnant** ("nearly impossible to use for dynamic values or complex logic"). `kubernetes-sigs/e2e-framework` is viable if you want everything in Go but has less momentum for CRD testing; ⚠️ sparse 2026 signal, which is absence of evidence rather than evidence of abandonment. Also notable: **Sawchain**, a Go library bringing Chainsaw's assertion engine into Ginkgo/envtest tests.

**Deterministic tests for rollout/canary timing** — the crux for a canary controller. Six concrete techniques:

1. **Inject a clock.** `k8s.io/utils/clock`: `clock.Clock` in production, `clock.NewFakeClock(t0)` in tests. **Never call `time.Now()` in reconcile** — enforce it with a `forbidigo` lint rule.
2. **Make the reconciler a pure state machine.** `next(spec, status, now) (desired, Result, error)` with zero I/O, exhaustively table-tested. This is where determinism actually comes from.
3. **Drive reconcile manually in integration tests.** Construct the reconciler against the envtest client and call `Reconcile(ctx, req)` N times, asserting between calls — no manager, no informers, no `Eventually`, no races.
4. **Use `Eventually` with explicit timeouts** only for the two or three manager-driven smoke tests that verify watch wiring.
5. **Parameterize all durations in the CRD spec** so tests can set them to milliseconds. Argo Rollouts does exactly this.
6. **Make the metrics provider an interface and script the fake.** A script of per-call responses ("pass, pass, fail, fail") lets you assert rollback fires on exactly the second failure, and lets you script provider *errors* to prove a monitoring outage doesn't cause a rollback — both impossible against a live Prometheus.

⚠️ **envtest runs no pods** — no kubelet, no Deployment controller, no ReplicaSets, no readiness. Every integration test must hand-fake `Deployment.status`. Write that helper once, early.

### 9.3 Load generation

| Tool | Fit |
|---|---|
| **`vllm-project/guidellm`** | SLO-aware LLM benchmarking, now part of the vLLM project. **Six load profiles: synchronous, concurrent, throughput, constant, poisson, sweep.** Measures TTFT and ITL. `constant`/`poisson` are exactly what's needed to drive an autoscaler deterministically. Red Hat has [a guide to running it on Kubernetes](https://developers.redhat.com/articles/2025/12/24/how-deploy-and-benchmark-vllm-guidellm-kubernetes). ⚠️ arm64 image not verified. |
| **`kubernetes-sigs/inference-perf`** | **SIG-blessed** (wg-serving), model-server agnostic, founded to standardize GenAI benchmark tooling across the k8s and model-server communities. Reports TTFT, TPOT, ITL, Normalized TPOT. ⚠️ arm64 image not verified. |
| `vllm bench serve` | Ships **inside** the vLLM image — zero extra images if vLLM is already in the cluster |
| `oha` / `fortio` / `hey` | Tiny multi-arch static binaries; not LLM-aware (no TTFT) but ideal for constant-RPS rollout-safety testing with clean p50/p95/p99 and error rates |
| **k6** (+ `xk6-sse`) | Needs the SSE extension for streaming; vanilla k6 works for non-streaming. `k6-operator` exists for distributed runs |

**Recommended pairing:** `oha`/`fortio` at fixed RPS as the rollout-safety driver (tiny, rock-solid, its error rate is a rollback trigger), plus `guidellm --rate-type poisson` for realistic bursty arrival and real TTFT. Note `inference-perf` in docs as the SIG-standard alternative.

⚠️ **Critical for TTFT correctness: the load generator must send `"stream": true`.** Non-streaming requests make TTFT meaningless.

### 9.4 Release engineering

- **golangci-lint v2.13.2.** The **v2 config format is mandatory** — the v1 parser isn't in the v2 binary. Key changes: an explicit `version: "2"` field; a new top-level **`formatters:`** section with `gci`/`gofmt`/`gofumpt`/`goimports` moved out of `linters`; `linters-settings` split into `linters.settings` and `formatters.settings`. `golangci-lint migrate` exists, but ⚠️ **comments are not migrated** and unknown v1 fields are dropped silently.
- **Distribution:** kustomize (`config/`) as source of truth, Helm chart generated from it via `kubebuilder edit --plugins=helm/v2-alpha`. **OLM: skip** — OLM v1 replaces CSV/Subscription with a declarative `ClusterExtension` API, but bundles add real complexity and signal "targeting OperatorHub."
- **Supply chain (2026 baseline):** cosign keyless signing via GitHub OIDC (`id-token: write`, no key management) · SLSA provenance via GitHub's native `actions/attest-build-provenance` (simpler and now more common than `slsa-github-generator`) · SBOM via `anchore/sbom-action` (Syft) attached with `cosign attest` · `govulncheck` plus Trivy/Grype.
- **Multi-arch:** `setup-qemu-action` + `setup-buildx-action` + `build-push-action` with `platforms: linux/amd64,linux/arm64`. **Cross-compile in Go rather than emulate** — QEMU-emulated `go build` is brutally slow. The Kubebuilder Dockerfile already does `FROM --platform=$BUILDPLATFORM golang AS builder` + `GOARCH=${TARGETARCH}`.
- **CI:** `helm/kind-action@v1.14.0` (2026-02-17) ⚠️ **defaults to kind v0.31.0 / k8s v1.35.0, which is behind kind v0.33.0** — pin `version:` and `node_image:` explicitly rather than relying on defaults. Pin all actions to commit SHAs and enable Dependabot for `github-actions`.
- **API docs:** **elastic/crd-ref-docs v0.3.0** (2026-02-02) — a re-implementation of `ahmetb/gen-crd-api-reference-docs` fixing Go-modules support and slow scans. Current best choice; wire into a `make api-docs` target and CI-check freshness.
- **Changelog:** Conventional Commits + `release-please` or `git-cliff`. (Kubebuilder itself uses emoji-prefixed commits, but standard Conventional Commits is the better choice for a new repo.)

---

## 10. Could not verify / sources conflict

Carried forward honestly, because several of these would change design decisions if resolved differently.

**Directly contradictory sources, resolved by re-verification:**

1. **llama.cpp linux/arm64 image.** Docs claimed it existed; issues [#13891](https://github.com/ggml-org/llama.cpp/issues/13891) and [#19177](https://github.com/ggml-org/llama.cpp/issues/19177) claimed it didn't. **Resolved: it exists** (`docker manifest inspect`, 2026-09-01). Fixed by PR #21207 merged 2026-03-31; the issues are stale.
2. **Flagger maintenance status.** Widely assumed to be winding down; **evidence contradicts this** (v1.45.0 published 2026-09-01). ⚠️ The CHANGELOG body was not read for maintenance statements — check directly before making a claim about it.
3. **Release dates.** GitHub's HTML releases pages returned 2025 dates via relative-timestamp misparse; the API returned 2026. **Trust the API.** Similarly, **Artifact Hub's release-date field is wrong** for kube-prometheus-stack (reported 2025-01-27 for chart 88.6.2, contradicting GitHub history) — use GitHub, not Artifact Hub, for dates.

**Not verified — check before depending on:**

4. **Whether Docker Desktop's arm64 VM exposes `FEAT_BF16`** to guests. Affects whether vLLM `--dtype=bfloat16` works on M2+. M1 definitively lacks it. Test with `docker run --rm --platform linux/arm64 alpine grep -o bf16 /proc/cpuinfo`.
5. **llama.cpp tokens/sec on arm64 inside Docker Desktop.** No primary measurement found for Qwen3-0.6B-Q4. Community numbers of 30–80 tok/s are plausible but unverified for this specific environment.
6. **`llamacpp:requests_deferred` semantics as a queue-depth proxy.** Undocumented in detail; may stay at 0 until all `--parallel` slots are busy. **Measure before wiring to an autoscaler.**
7. **Whether llama.cpp metrics-format issues #17384 / #19811 affect current builds.** Not read in full.
8. **Published linux/arm64 images for `guidellm`, `inference-perf`, `oha`.** Check `docker manifest inspect` for each; worst case a five-line Dockerfile over `python:3.12-slim` covers the Python ones.
9. **Whether ImageVolume is GA in Kubernetes 1.36.** Secondary sources only; beta-enabled-by-default in 1.35 is solid.
10. **Whether Docker Hub `ai/*` ModelPack artifacts** (`application/vnd.cncf.model.manifest.v1+json`) mount cleanly via k8s ImageVolume.
11. **Grafana Foundation SDK GA status.** Grafana's docs call it production-ready and say Dashboard V2 is stable in Grafana 13+, but **no explicit GA announcement or `v1.0.0` Go module tag was found.** Treat "stable" as claim, not proof.
12. **What Grafana appVersion the kube-prometheus-stack grafana subchart 12.11.2 pins** — likely 12.x, not 13.x. Check before promising "Grafana 13".
13. **`workqueue_*` metric availability.** Two open upstream issues report registration collisions and silently missing series. `curl` `/metrics` before dashboarding them.
14. **`gemma-3-270m` exact license** — sources conflict between Apache-2.0 and Gemma Terms. Gated either way, so it's excluded regardless.
15. **`grafana/otel-lgtm` current tag.**
16. **llama.cpp OTel tracing** — no evidence it exists as of 2026-09. Absence of evidence, not proof of absence.
17. **kind v0.33.0 arm64 node images for every version.** ✅ `kindest/node:v1.35.8` was re-verified as multi-arch; v1.36.4 and v1.37.0 were not individually checked.
18. **CRDValidationRatcheting exact GA milestone.** Beta-and-default-on in 1.30 is certain; the precise GA milestone was not pinned. Irrelevant on 1.35+.
19. **controller-runtime and `slog`.** No evidence of first-class adoption; logr/zap remains the scaffold default. Stated as "no evidence found," not "confirmed absent."
20. **`defilantech/LLMKube`** — a third-party operator that may substantially overlap this project's premise. **Review before starting.**
21. **OpenKruise Rollouts** (`openkruise/rollouts`) — only core Kruise releases were checked. Worth a look as a third data point on canary vocabulary.
22. **Kubebuilder v4.15.0's exact controller-tools and kustomize pins** — reported as v0.21.0 and v5.8.1 from release notes and plugin docs; confirm by reading the generated `Makefile` after `kubebuilder init`.

---

## 11. Recommended stack — summary

| Concern | Choice | Version |
|---|---|---|
| Scaffold | Kubebuilder | v4.15.0 → controller-runtime **v0.24.1**, controller-tools v0.21.0 |
| Language | Go | 1.26 |
| Cluster | kind (upgrade from v0.31.0) → `kindest/node:v1.35.8` | v0.33.0 |
| Image delivery | local registry on `localhost:5001` | — |
| Engine (default) | `ghcr.io/ggml-org/llama.cpp:server-b<pinned>` | arm64 verified |
| Engine (second) | `vllm/vllm-openai-cpu:v0.28.0-arm64` | v0.28.0 |
| Model | `unsloth/Qwen3-0.6B-GGUF` / `Qwen3-0.6B-Q4_K_M.gguf` | 397 MB, Apache-2.0, ungated |
| RED + TTFT metrics | **own Go reverse-proxy sidecar** (llama.cpp has no histograms) | — |
| Metrics backend | kube-prometheus-stack, slimmed, all `*SelectorNilUsesHelmValues: false` | chart 88.6.2 |
| Scrape config | operator-created `ServiceMonitor` (`monitoring.coreos.com/v1`), optional-CRD guarded | — |
| Operator metrics | `metrics.Registry` + `filters.WithAuthenticationAndAuthorization` on `:8443` | — |
| Autoscaling | `/scale` subresource + own target-tracking loop on **queue depth (target 3–5)**; KEDA as the delegated option | KEDA v2.20.2 |
| Traffic split | replica-weighted (zero deps); per-request proxy or Gateway API `HTTPRoute` weights if exactness is needed | — |
| Canary | **own controller**, Flagger-shaped vocabulary + four-valued verdicts | Flagger v1.45.0 / Argo v1.10.0 as references |
| Rollback history | `ControllerRevision` (apps/v1) | — |
| Dashboards | hand-written JSON → `//go:embed` → ConfigMap `grafana_dashboard: "1"` | — |
| SLO alerts | hand-written MWMB `PrometheusRule`; Sloth for the rest | — |
| Unit/integration tests | plain Go table tests + envtest (native darwin/arm64) | envtest v1.36+ |
| E2E | Kyverno Chainsaw | v0.2.15 |
| Lint | golangci-lint, v2 config | v2.13.2 |
| API docs | elastic/crd-ref-docs | v0.3.0 |

**Rejected, each worth an ADR:** SGLang · TGI · Ollama · ingress-nginx · prometheus-adapter · Knative · ImageVolume-on-kind · Argo Rollouts/Flagger as a dependency · OLM bundles · gemma-3-270m and Llama-3.2-1B (gated).
