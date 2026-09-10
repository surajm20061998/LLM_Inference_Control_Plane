# LLMCP Improvement Plan

- Status: Implementation in progress; see [execution status](docs/implementation-status.md)
- Last researched: 2026-09-05
- Code baseline: `69c8e35`, audited after resuming the interrupted research
- Scope: README presentation, correctness fixes, and a staged product roadmap. This document proposes work; it does not claim that the proposed capabilities are implemented.

**Start with [the executable delivery program](#15-executable-delivery-program), then [the PR specifications](#16-pr-specifications).** Sections 1–11 explain the problems and design rationale. Sections 15–19 define the execution order, implementation contracts, upgrade behavior, verification commands, and customer demonstrations. Where the earlier proposals leave alternatives open, the execution program supplies a recommended choice for review.

## 1. Product statement to preserve

LLMCP is a Kubernetes-native release-safety and elasticity controller for self-hosted, OpenAI-compatible LLM inference.

Its strongest implemented use case is not merely deploying a model. It is detecting and stopping a candidate revision that remains Kubernetes-healthy and returns successful responses but makes the user experience worse, particularly through regressed time to first token (TTFT).

The README should consistently describe the project as providing **operational rollout safety**:

- serving availability and readiness;
- TTFT and request-duration regression detection;
- HTTP 5xx gating;
- queue-depth/concurrency autoscaling;
- durable promotion, abort, and rollback state;
- Prometheus/Grafana observability.

It must not imply that the controller currently evaluates factuality, hallucination, toxicity, policy compliance, or general response quality.

## 2. Current implementation baseline

The following facts are the source of truth for the README and the proposed changes.

| Concern | Implemented behavior | Source |
|---|---|---|
| Public API | One namespaced `ModelDeployment` CRD owns model, engine, serving, rollout, observability, and autoscaling policy | [`api/v1alpha1/modeldeployment_types.go`](api/v1alpha1/modeldeployment_types.go#L37-L88) |
| Child resources | The controller renders primary/canary Deployments, a shared serving Service, and a headless metrics Service | [`internal/controller/children.go`](internal/controller/children.go#L139-L184) |
| Reconciliation | Validation, revision recording, autoscaling, rollout planning, child apply, observability, and status happen in that order | [`internal/controller/modeldeployment_controller.go`](internal/controller/modeldeployment_controller.go#L135-L279) |
| Model delivery | OCI image weights are copied through an init container; Hugging Face weights are downloaded by llama.cpp into `emptyDir` | [`internal/controller/children.go`](internal/controller/children.go#L297-L417) |
| PVC source | The API models a PVC source, but llama.cpp validation rejects it as not implemented | [`internal/engine/llamacpp.go`](internal/engine/llamacpp.go#L94-L136) |
| Engine support | Only `llamacpp` is accepted by the API | [`api/v1alpha1/engine_types.go`](api/v1alpha1/engine_types.go#L23-L35) |
| Request path | The serving Service sends traffic through a native-sidecar reverse proxy and then to the engine over loopback | [`internal/controller/shim.go`](internal/controller/shim.go#L77-L151), [`cmd/shim/proxy.go`](cmd/shim/proxy.go#L160-L214) |
| Decision metrics | The shim emits canonical `llmcp_*` TTFT, duration, request, queue, concurrency, and upstream-error metrics | [`cmd/shim/metrics.go`](cmd/shim/metrics.go#L95-L179) |
| Metric identity | Workload metrics currently carry only `model` and `variant` as constant identity labels | [`cmd/shim/metrics.go`](cmd/shim/metrics.go#L95-L98) |
| Canary analysis | Low traffic short-circuits to `Inconclusive`; results are classified as Pass, Fail, Inconclusive, or Error | [`internal/analysis/analyzer.go`](internal/analysis/analyzer.go#L53-L127), [`internal/analysis/evaluate.go`](internal/analysis/evaluate.go#L38-L135) |
| Traffic routing | The only implemented routing mode approximates traffic using primary/canary replica counts behind one Service | [`api/v1alpha1/rollout_types.go`](api/v1alpha1/rollout_types.go#L184-L215) |
| Autoscaling | The built-in controller chooses a total replica count from queue depth or concurrency and the canary code divides that total | [`internal/autoscale/recommend.go`](internal/autoscale/recommend.go#L177-L299), [`internal/canary/weights.go`](internal/canary/weights.go#L19-L85) |
| SLOs | The operator generates TTFT and availability recording rules, multi-window burn alerts, and a no-traffic alert | [`internal/observability/prometheusrule.go`](internal/observability/prometheusrule.go#L121-L155) |
| Dashboards | Three Grafana dashboards are embedded in the manager image and installed as dashboard ConfigMaps | [`internal/observability/dashboards.go`](internal/observability/dashboards.go#L36-L49) |

## 3. Guiding principles

1. Every headline README claim must link to an implementation file, an ADR, a reproducible command, or a captured result.
2. Present behavior and roadmap behavior must be visually separated.
3. Fix incorrect behavior before making diagrams or screenshots that depend on it.
4. Preserve the controller's ownership contract: autoscaling chooses total capacity; rollout routing decides exposure.
5. Missing measurements must never be silently converted into zero or success.
6. Optional integrations must fail visibly without preventing an otherwise healthy model from serving.
7. New metric labels must be bounded by Kubernetes resources, not requests, users, prompts, or other unbounded data.
8. Prefer staged improvements that leave the current zero-dependency replica-routing mode useful.

## 4. Workstream A: README and GitHub presentation

### Goal

Replace the current README with a product-first, implementation-grounded explanation that a platform engineer can understand before reading the CRD or source.

GitHub renders Mermaid diagrams directly in Markdown, so the diagrams should remain text-reviewable and change with the code instead of becoming stale binary artwork. GitHub documents Mermaid support for repository Markdown files in [Creating diagrams](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/creating-diagrams).

### Proposed README structure

1. **Hero section**
   - Name and one-sentence value proposition.
   - A short statement of the failure it catches: “healthy but slower.”
   - Badges only for workflows and artifacts that actually exist.
   - Links to Quick start, Demo, Architecture, API, and Limitations.

2. **The problem**
   - Explain why readiness and rolling updates cannot detect TTFT regressions.
   - Explain why queue depth is a more direct serving-pressure signal than CPU for this implementation.
   - State that safety means operational safety, not semantic answer quality.

3. **End-to-end architecture**
   - Mermaid flowchart showing `ModelDeployment`, controller, revisions, primary/canary Deployments, serving and metrics Services, native sidecar, llama.cpp, Prometheus, rollout analysis, autoscaling, rules, and dashboards.
   - Distinguish the request path from the control/measurement path by edge style or color.

4. **What happens during a rollout**
   - Mermaid sequence diagram: spec change → revision record → canary creation → readiness/warm-up → Prometheus query → verdict → advance/promote/rollback → status/Event.
   - Include a note that the first deployment cannot be canaried because no last-good revision exists.

5. **Decision semantics**
   - A compact diagram or table for Pass, Fail, Inconclusive, and Error.
   - Document what each counter does and what happens when its limit is reached.
   - Avoid the absolute claim that a monitoring outage “can never cause rollback.” The accurate claim is that provider errors do not spend the failed-check budget; the controller holds initially and aborts the rollout after the configured consecutive-error limit.

6. **Autoscaling ownership**
   - Diagram: Prometheus fleet metric → desired total → primary/canary split.
   - Explain that `/scale`, `kubectl scale`, external HPA, and the built-in autoscaler all operate on the same total-replica field.

7. **Model delivery and engine runtime**
   - OCI image and Hugging Face flows.
   - State clearly that OCI is recommended, Hugging Face re-downloads into ephemeral storage, PVC is not implemented, and only llama.cpp is supported.

8. **Customer scenarios**
   - Healthy-but-slow upgrade.
   - Queue-driven burst scaling.
   - Private/on-prem OCI model delivery with manual approval.

9. **Quick starts**
   - A fast deterministic path using `fakeengine`.
   - A separate real-model path using Qwen3/llama.cpp.
   - Do not mix fake-engine environment variables into commands described as a real llama.cpp demo.

10. **Current scope and limitations**
    - Replica-based traffic distribution is approximate.
    - Operational metrics only.
    - llama.cpp only.
    - No scale-to-zero, public gateway, persistent Hub cache, or authenticated Prometheus provider yet.

11. **Documentation map and roadmap**
    - Link ADRs, API reference, SLO runbook, demo, and this plan.
    - Use a small “Implemented / Planned” table instead of unqualified future-facing prose.

### Required diagrams

#### Diagram 1: system architecture

Ground it in `buildChildren`, `buildShimContainer`, the reconciliation order, and the observability builders. It should show only resources that the controller actually creates.

#### Diagram 2: request and metric flow

Show:

```text
client → ClusterIP Service → shim:8080 → llama.cpp:8000
                               └──────→ metrics:9090 → headless Service → Prometheus
```

The engine port must not be shown as exposed through the serving Service because the implementation intentionally prevents that bypass.

#### Diagram 3: canary lifecycle

Show first deployment, canary start, readiness/warm-up, analysis, pause, promotion, abort, rollback, and the sticky failed revision.

#### Diagram 4: verdict behavior

Show the separate paths and counters for measurement failure, insufficient evidence, and measured regression.

#### Diagram 5: capacity versus exposure

Show that the autoscaler owns the total and the routing implementation divides it. For replica routing, explicitly show integer quantization.

### Figures and screenshots

After the correctness work below:

- add `docs/assets/canary-rollback.png` showing primary/canary TTFT and the rollback annotation;
- add `docs/assets/queue-autoscaling.png` showing queue depth and replica response;
- add `docs/assets/modeldeployment-status.png` showing useful status fields and conditions;
- capture figures from a committed sample configuration and record that sample in each caption;
- provide descriptive alt text;
- do not use mock dashboard values without marking them as illustrative.

### README acceptance criteria

- Mermaid blocks render on GitHub without third-party extensions.
- Every architecture node maps to a real resource or process.
- Every “automatic” behavior links to its controller/state-machine implementation.
- All example outputs are captured from, or mechanically derived from, a committed sample.
- Implemented and planned capabilities are visibly distinct.
- The limitations section is reachable from the top of the README.
- The 10-minute demo and README do not contradict one another.

## 5. Workstream B: make replica-based canary ladders truthful

### Problem

The original flagship sample requested `[25, 50, 75]` but fixed the canary at one replica. The sample override was removed first. At the audit baseline, the underlying API/controller issue remained: the replica override ignored the requested weight, leaving a four-replica deployment at 3 primary / 1 canary, or 25%, at every rung.

The API description of fixed canary scale assumes traffic can be controlled independently, but the only current traffic mechanism is replica count.

### Recommended immediate design

For `trafficRouting.mode: Replica`:

1. Always calculate replicas with `Split(total, desiredWeight)`.
2. Reject or ignore-with-an-explicit-condition `canary.scale.replicas`; rejection is preferred because silently accepting it preserves the current contradiction.
3. Treat `matchTrafficWeight: true` as the only meaningful replica-mode behavior.
4. Preserve the removal of `scale.replicas: 1` from `config/samples/canary_qwen3.yaml` (already completed).
5. Update the expected ladder to the actual four-replica splits:
   - 25% → primary 3, canary 1;
   - 50% → primary 2, canary 2;
   - 75% → primary 1, canary 3.
6. Keep reporting desired and mathematically configured weight separately.

Because the canary Deployment will grow between steps, the availability contract also needs tightening:

- pass the canary's ready-replica count into the state machine rather than a Boolean “at least one is ready”;
- require the ready count for the current rung before analysis;
- reset the rung's warm-up timestamp when its desired canary replica count changes;
- do not present desired replica weight as active exposure while new endpoints are still loading.

### API compatibility choice

`CanaryScaleSpec` can remain in `v1alpha1` for the future Gateway mode, but validation should state that fixed replicas require a routing mode capable of independent weighting. Since `Replica` is currently the only enum value, the immediate behavior is effectively “field reserved but unsupported.”

### Verification

- State-machine cases for all ladder steps and ready-count transitions.
- Property check that primary + canary always equals total.
- Controller check that analysis does not run until all canary replicas for the rung are ready.
- Sample validation proving no fixed override is combined with replica routing.
- Demo output generated from the real status fields rather than hand-written expectations.

### Acceptance criteria

- `desiredWeight=25/50/75` produces 3/1, 2/2, and 1/3 with four replicas.
- No supported configuration claims to decouple canary pods from traffic while replica count is the router.
- Analysis never runs against a partially ready new rung.
- README and `docs/demo.md` report the same values produced by status.

## 6. Workstream C: scope decision metrics to a ModelDeployment

### Problem

Canary and autoscaling PromQL currently select only `model` and `variant`, while SLO recording rules aggregate only by `model`. Two resources serving the same model name can influence each other's rollout, scaling, alerts, and dashboards.

Prometheus recommends labels when a dimension is required to identify or aggregate a metric, while warning against unbounded cardinality. Namespace and ModelDeployment name are bounded by the number of deployed resources and are necessary for correctness; request IDs, user IDs, prompt text, and pod UIDs are not appropriate identity labels. See Prometheus's [instrumentation guidance](https://prometheus.io/docs/practices/instrumentation/) and [metric and label naming guidance](https://prometheus.io/docs/practices/naming/).

### Recommended design

Add two canonical labels to every decision metric:

```text
namespace
model_deployment
```

Keep `model` for human aggregation and `variant` for primary/canary comparison.

Implementation steps:

1. Add label-name constants in `internal/metrics/names.go`.
2. Add required `--namespace` and `--model-deployment` shim configuration.
3. Pass `md.Namespace` and `md.Name` from `shimArgs`.
4. Add the labels to all shim collectors, including `llmcp_shim_info`.
5. Extend `analysis.QueryContext` with namespace and resource name.
6. Update built-in canary, request-rate, ratio, and fleet autoscaling selectors.
7. Add `{{.Namespace}}` and `{{.ModelDeployment}}` substitutions for custom PromQL while retaining existing variables.
8. Scope Prometheus recording rules and alert labels by namespace and resource name.
9. Update Grafana variables and panel queries to select a concrete ModelDeployment.
10. Update metric documentation and dashboard legends.

Use labels emitted directly by the shim rather than relying only on ServiceMonitor target relabeling. This keeps the identity correct if a user scrapes the endpoint without the generated ServiceMonitor.

### Migration notes

- Old pods will continue emitting the old label set until rolled.
- The controller change modifies shim arguments in the pod template, causing serving pods to restart on operator upgrade even if the user specification is unchanged. This behavior and its rollout impact must be documented.
- Queries should not temporarily fall back to unscoped series because that would reintroduce cross-resource contamination.
- Dashboard variables should hide or clearly separate legacy unscoped series during the transition.

### Verification

- Two resources with the same `spec.model.name` in different namespaces produce disjoint PromQL results.
- Two resources with the same model in one namespace remain disjoint by resource name.
- Autoscaling one resource cannot read the other's queue.
- SLO recording-rule names may be shared, but their output label sets must remain unique.
- Cardinality tests ensure no request-derived values become labels.

### Acceptance criteria

- Every automated query includes namespace and ModelDeployment selectors.
- Same-model deployments cannot affect one another's decisions.
- Dashboard selection uniquely identifies a resource.
- Raw-query users have documented identity template variables.

## 7. Workstream D: fix monitoring reconciliation truthfulness

### Immediate bug

Change the ServiceMonitor apply branch from testing `err` to testing `applyErr` in [`internal/controller/observability.go`](internal/controller/observability.go#L162-L170).

On failure:

- return the wrapped apply error so reconciliation retries;
- report `MetricsRegistered=False` with the apply error;
- do not report metrics as registered;
- do not prevent the existing serving workload from continuing.

### Adjacent reconciliation gap

Optional monitoring CRDs and their children are not continuously watched. A steady resource with no canary or autoscaler timer may not notice that prometheus-operator was installed later or that a generated monitoring child was deleted.

Recommended staged approach:

1. Add `RequeueAfter` to the observability verdict.
2. Retry discovery quickly, for example after the existing negative-discovery TTL, while an enabled CRD is absent.
3. Reconcile enabled observability resources on a slower periodic resync even when healthy.
4. Include observability in the controller's `soonest(...)` scheduling calculation.
5. Later evaluate a dynamic watch for optional unstructured resources; do not make manager startup depend on an absent CRD.

### Verification

- Inject a ServiceMonitor apply failure and assert `MetricsRegistered=False`, an error return, and no false success.
- Install the optional CRD after a ModelDeployment reaches steady state and verify the resource appears without touching the ModelDeployment.
- Delete or mutate a generated ServiceMonitor/PrometheusRule and verify it is restored.
- Verify clusters without prometheus-operator continue serving and do not enter a hot reconciliation loop.

### Acceptance criteria

- Apply failures are observable in status and logs.
- Optional-CRD absence is reported factually.
- Later installation and child drift converge without an unrelated workload event.

## 8. Workstream E: separate requested, configured, and observed traffic exposure

### Immediate clarification

The current `status.canary.currentWeight` is derived from desired replica counts. It is not a measurement of the fraction of requests that reached the candidate. Kubernetes Service routing and client connection reuse can cause the request distribution to differ from the pod ratio.

Until a new API is introduced:

- document `currentWeight` as **configured replica share**;
- never call it “measured traffic” or “actual request percentage”;
- show the connection-level limitation next to replica-routing examples;
- use a single 25% rung for the first polished rollback demo if repeatable multi-rung readiness cannot yet be guaranteed.

### Add observed exposure

After metric scoping is fixed, add a query over request rates for both variants:

```text
observedWeight = canaryRequestRate / (primaryRequestRate + canaryRequestRate)
```

Expose it separately, for example as `status.canary.observedWeight`, together with the window and timestamp. Missing or insufficient traffic must produce an unknown/empty observation, not zero.

Keep these concepts distinct:

- `desiredWeight`: the state-machine target;
- `currentWeight`: the routing configuration or replica-derived share;
- `observedWeight`: sampled request distribution from Prometheus.

### Request-weighted routing with Gateway API

Gateway API `HTTPRoute` supports multiple backend references with weights; the effective share is each weight divided by the sum of weights. The official reference includes a 90/10 example: [Gateway API HTTPRoute](https://gateway-api.sigs.k8s.io/reference/api-types/httproute/).

Recommended future mode:

```yaml
trafficRouting:
  mode: GatewayAPI
  gatewayAPI:
    gatewayRef:
      name: inference-gateway
      namespace: gateway-system
    hostnames:
      - chat.example.com
```

Implementation outline:

1. Add separate primary and canary Services whose selectors include `variant`.
2. Create or manage an `HTTPRoute` with both Services as `backendRefs`.
3. Set backend weights from the canary state machine rather than changing pod counts.
4. Read route `Accepted` and `ResolvedRefs` for the intended parent and current generation. Check Gateway/listener programming and any additional route conditions supported by the pinned implementation. Status acceptance alone does not prove that all data-plane instances have applied a weight change; see PR-09 below.
5. Allow fixed canary replicas only for this independent-routing mode.
6. On rollback, restore stable routing before draining the candidate. On promotion, prepare and verify the replacement primary before moving its traffic or deleting the candidate; see the staged handoff in PR-09 below.
7. Discover Gateway API as an optional dependency and report `TrafficRoutingReady` accurately when it is absent.
8. Decide whether the operator owns the `HTTPRoute` or patches a user-owned route. Operator ownership is recommended initially because it gives one clear reconciliation contract; the Gateway itself should remain user-supplied infrastructure.

Gateway weights configure request forwarding more accurately than pod ratios, although implementations may still have a small precision error, as the [Gateway API specification](https://gateway-api.sigs.k8s.io/reference/api-spec/main/spec/) notes.

### Acceptance criteria

- Status never presents a mathematical pod ratio as a measured request ratio.
- Replica mode remains dependency-free and explicitly approximate.
- Gateway mode can change 25→50→75 traffic without resizing a fixed one-pod canary.
- Routing readiness gates analysis.
- Rollback restores stable routing before deleting the candidate.

## 9. Workstream F: define operational safety and add a path to semantic evaluation

### Phase 1: documentation now

Use “operational rollout safety” throughout the README. List the exact built-in signals from [`analysis_types.go`](api/v1alpha1/analysis_types.go#L188-L225).

Add a prominent statement:

> LLMCP determines whether a candidate is operationally worse according to configured metrics. It does not currently determine whether an answer is factually correct, safe, unbiased, or useful.

Document custom PromQL as an integration point, not as a built-in semantic evaluator.

### Phase 2: external semantic signals

Define and document a contract for an external evaluator to publish bounded, resource-scoped metrics such as:

- answer-quality pass ratio;
- safety-policy violation ratio;
- retrieval-groundedness score;
- evaluation sample count.

The canary can consume these through existing custom PromQL after metric identity is fixed. Require a minimum evaluation sample count so a candidate cannot pass on one or zero scored responses.

### Phase 3: first-class evaluation workflow

Two complementary designs should be prototyped before changing the API:

1. **Pre-promotion evaluation Job**
   - The controller creates an owned Kubernetes Job for a named dataset/evaluator.
   - The Job calls the candidate endpoint and reports a structured result.
   - Promotion waits for Job completion and threshold evaluation.
   - Kubernetes Jobs are the native primitive for one-off work that runs to completion; see the [Kubernetes Job documentation](https://kubernetes.io/docs/concepts/workloads/controllers/job/).

2. **Opt-in shadow evaluation**
   - Duplicate a controlled portion of traffic to a candidate/evaluator without using its response for the client.
   - Gateway API provides a `RequestMirror` filter and specifies that the mirrored response is ignored; see [HTTP request mirroring](https://gateway-api.sigs.k8s.io/guides/user-guides/http-request-mirroring/).
   - Request mirroring alone cannot score the ignored candidate response. A dedicated shadow/evaluation component must capture candidate output, apply privacy controls, and publish aggregate results.

Risks that must be designed explicitly:

- doubled inference cost and load;
- prompts containing sensitive information;
- non-idempotent tool calls or side effects;
- judge-model instability and versioning;
- delayed semantic results versus rollout deadlines;
- storage and retention of prompts/responses.

### Acceptance criteria

- Current documentation makes no semantic-safety claim.
- Custom quality gates require sample-count evidence.
- Any future first-class evaluator versions its dataset, evaluator, and threshold in status/history.
- Shadowing is off by default and has explicit privacy and side-effect warnings.

## 10. Workstream G: grow deliberately beyond a single-engine alpha

This is a roadmap, not a README claim.

### G1. vLLM engine profile

Implement a second `engine.Profile` that supplies:

- a pinned image or digest policy;
- OpenAI-compatible server arguments;
- health and metrics endpoints;
- supported model-source validation;
- model-download credentials through Secrets, with client authentication documented through the selected Gateway installation;
- GPU runtime configuration first, with CPU variants requiring separate validation;
- canonical shim behavior unchanged from llama.cpp.

vLLM documents an OpenAI-compatible HTTP server and metrics APIs in its official [online serving documentation](https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html). The point of this work is to prove that the engine abstraction and canonical shim metrics survive a real engine swap.

Do not gate rollout decisions directly on engine-native metric names. Continue using the shim contract for cross-engine metrics, with native vLLM metrics available for diagnostics.

### G2. PVC model delivery

Implement the existing `persistentVolumeClaim` union member by:

1. adding a `PersistentVolumeClaimVolumeSource` to the pod;
2. mounting it read-only at a deterministic runtime directory;
3. resolving and validating the configured file path without an init copy;
4. including relevant mount settings in revision identity;
5. documenting required access modes for multiple replicas and multi-node clusters.

Kubernetes defines PVCs as durable storage requests with a lifecycle independent of an individual pod; see [Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/). A shared multi-replica deployment must not assume that every storage class supports ReadOnlyMany or ReadWriteMany.

### G3. Persistent Hugging Face cache

Add an optional cache volume configuration rather than silently replacing the current `emptyDir` behavior. Possible modes:

- ephemeral `emptyDir` as today;
- user-supplied PVC;
- later, an operator-created claim template.

Document concurrent download and access-mode behavior. OCI model images should remain the recommended immutable/offline production path.

### G4. Workload placement and accelerators

Add a controlled pod-template subset:

- `nodeSelector`;
- tolerations;
- affinity;
- topology spread constraints;
- priority class;
- runtime class if required by accelerator stacks.

The existing resource map can already represent extended resource names, but the project needs an engine/image that can use them and placement fields to target suitable nodes. Kubernetes exposes GPUs through device-plugin extended resources such as `nvidia.com/gpu`; see [Schedule GPUs](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/). Node affinity and topology spread are the standard placement mechanisms; see [Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/) and [Pod Topology Spread Constraints](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/).

All pod-template-affecting fields must be added deliberately to revision identity.

### G5. Public ingress and authenticated Prometheus

Public exposure should build on Gateway API rather than adding a separate, unrelated Ingress abstraction after Gateway-based traffic routing exists.

Extend `AnalysisProviderSpec` with Secret-backed authentication and TLS configuration shared by canary and autoscaling providers:

- bearer token or authorization credentials;
- optional basic authentication;
- CA bundle and server-name configuration;
- optional client certificate/key;
- explicitly allowed static tenancy headers.

Never place credentials in arguments, Events, status, or logs. The Prometheus Operator API demonstrates Secret-based authorization and TLS configuration patterns in its [API reference](https://prometheus-operator.dev/docs/api-reference/api/), although LLMCP's query client will need its own transport implementation.

### G6. Scale to zero

Do not implement scale-to-zero by merely changing `minReplicas` to zero. When no shim pod exists, its queue/concurrency metric also disappears, so there is no signal that can reactivate the deployment.

Scale-to-zero requires an always-available request/activation layer or an external event source that can observe demand while the model is absent. KEDA similarly notes that CPU or memory triggers cannot activate from zero when there are no running pods producing those metrics; see [KEDA concepts](https://keda.sh/docs/2.20/concepts/).

Recommended sequence:

1. implement a Gateway/router path;
2. define request queueing and maximum activation wait;
3. expose an activation signal independent of serving pods;
4. model cold-start and failure behavior;
5. only then permit `minReplicas: 0`.

### Acceptance criteria for broader product maturity

- A second real engine completes the same operational canary scenario.
- PVC-backed deployments work across documented access modes.
- accelerator placement is represented without exposing an unrestricted raw PodSpec.
- provider credentials remain Secret-backed and redacted.
- scale-to-zero has a functioning activation path and bounded request wait.

## 11. Additional correctness items discovered during review

These are not part of the original six points, but should be considered before calling the API production-ready.

### Revision identity

- `serving.startupTimeout` changes the rendered pod probes but is excluded from the revision hash.
- `serving.port` is included in the revision hash even though it changes the Service rather than the pod workload.

Audit every field using one rule: if it changes the candidate pod template or runtime behavior being compared, it belongs in revision identity; if it only changes rollout policy or a stable external Service, it does not.

### Engine concurrency contract

`engine.extraArgs` can override llama.cpp `--parallel`, while the shim still derives queue depth from `spec.engine.maxConcurrency`. Either prevent overrides of controller-owned flags or parse/resolve one authoritative effective value and pass it to both engine and shim.

### Output-token metrics

The shim currently counts content-bearing SSE frames, which are not guaranteed to equal tokenizer tokens. Rename the metric to reflect chunks, or implement protocol/engine-supported token accounting before using it for billing or precise throughput claims.

### Monitoring and dashboard drift

- Generated monitoring children need continuous reconciliation as described above.
- Dashboard burn-rate calculations currently assume default objectives in places; parameterized objectives require generated dashboard values or variables.
- API comments that still say Hugging Face is unimplemented must be corrected.

## 12. Proposed delivery order

This was the initial workstream ordering. Use the dependency-aware PR sequence in section 15 for execution, including the additional correctness work in section 11.

| Order | Work package | Priority | Relative size | Why now |
|---|---|---:|---:|---|
| 1 | ServiceMonitor apply-error fix | P0 | Small | Removes a direct false-success condition |
| 2 | Resource-scoped metric identity and queries | P0 | Medium | Required for correctness whenever model names repeat |
| 3 | Replica-mode ladder semantics and readiness | P0 | Medium | Makes the flagship behavior and sample truthful |
| 4 | Immediate traffic/safety documentation corrections | P0 | Small | Prevents misleading external claims |
| 5 | README rewrite with Mermaid diagrams | P1 | Medium | Presentation should be built on corrected behavior |
| 6 | Capture real dashboard/status figures | P1 | Small/Medium | Adds credible visual proof after behavior is stable |
| 7 | Observed traffic-share status | P1 | Medium | Makes routing behavior empirically visible |
| 8 | Optional monitoring resync/drift repair | P1 | Medium | Improves long-running operator behavior |
| 9 | Gateway API weighted routing | P2 | Large | Enables true traffic/replica decoupling and public routing |
| 10 | Prometheus authentication and TLS | P2 | Medium | Needed in secured and managed environments |
| 11 | PVC and persistent Hub cache | P2 | Medium | Removes repeated model downloads and supports existing storage |
| 12 | vLLM plus placement/GPU controls | P2 | Large | Expands the product beyond its current llama.cpp scope |
| 13 | Semantic evaluation integration | P3 | Large | Expands “operational safety” into model-quality evidence |
| 14 | Scale-to-zero activation architecture | P3 | Large | Requires a router/activator, not only an autoscaler change |

## 13. Review decisions requested

Recommended choices for review (these are incorporated into the execution program; review does not require answering each item separately):

- [ ] In replica-routing mode, reject fixed `canary.scale.replicas` and always match replica share to desired weight.
- [ ] Add `namespace` and `model_deployment` directly to shim metrics rather than depending on ServiceMonitor-added target labels.
- [ ] Keep `currentWeight` for configured share and add a separate `observedWeight`, avoiding a breaking rename during `v1alpha1`.
- [ ] Make a future Gateway mode own the `HTTPRoute` while referencing a user-managed `Gateway`.
- [ ] Describe the current guarantee as operational rollout safety and defer first-class semantic evaluation until after core routing/identity work.
- [ ] Use vLLM as the second engine profile.
- [ ] Keep OCI model images as the recommended production source after PVC/cache support is added.

## 14. Primary research references

- [GitHub: Creating diagrams](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/creating-diagrams)
- [Gateway API: HTTPRoute](https://gateway-api.sigs.k8s.io/reference/api-types/httproute/)
- [Gateway API specification: backend weights](https://gateway-api.sigs.k8s.io/reference/api-spec/main/spec/)
- [Gateway API: HTTP request mirroring](https://gateway-api.sigs.k8s.io/guides/user-guides/http-request-mirroring/)
- [Prometheus: Instrumentation practices](https://prometheus.io/docs/practices/instrumentation/)
- [Prometheus: Metric and label naming](https://prometheus.io/docs/practices/naming/)
- [Prometheus Operator API reference](https://prometheus-operator.dev/docs/api-reference/api/)
- [Kubernetes: Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
- [Kubernetes: Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [Kubernetes: Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
- [Kubernetes: Pod Topology Spread Constraints](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/)
- [Kubernetes: Schedule GPUs](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/)
- [vLLM: OpenAI-compatible online serving](https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html)
- [KEDA: Scaling concepts](https://keda.sh/docs/2.20/concepts/)

## 15. Executable delivery program

### Fresh baseline: what still needs implementation

This audit is against `69c8e35`. It supersedes interrupted progress notes. In particular, the ServiceMonitor branch declares `applyErr` but **still tests `err`**; the error fix is not complete.

| Item | Verified state | Execution consequence |
|---|---|---|
| README and diagrams | Product-oriented README/assets exist; the sequence-diagram semicolon was already replaced | Preserve the rendering fix, add regression validation, and re-capture evidence after the correctness release |
| Sample ladder | Fixed canary replica override was removed from `config/samples/canary_qwen3.yaml` | Preserve this fix; controller validation and per-rung readiness still need work |
| ServiceMonitor apply | Wrong variable remains at `internal/controller/observability.go:165` | PR-01 is still required |
| Canary readiness | `internal/controller/canary.go:261` supplies `ReadyReplicas > 0`; `internal/canary/state.go` persists `AvailableSince` | Replace the Boolean contract and reset evidence timing when a rung or replica target changes |
| Metric scope | `analysis.QueryContext` contains Model/Variant/Window; shim constant labels contain model/variant | Migrate the producer, all automated consumers, rules, and dashboards together |
| Monitoring resync | `Reconcile` returns the minimum of rollout and autoscaler timers; discovery TTLs are 30 seconds absent / 10 minutes present | A discovery cache expiry does not itself schedule reconciliation; add a timer |
| Stream accounting | `cmd/shim/sse.go` increments its token count once per content-bearing frame; request counter increments on completion | Correct chunk/token terminology; choose an explicit exposure measurement contract |
| Engine and storage | `engine.Profile` already has Validate/Build/DefaultImage; llama.cpp uses `LLAMA_CACHE`; PVC is rejected | Extend these seams; do not assume Python Hub cache settings apply to the pinned llama.cpp image |
| Revision payload | `internal/revision/hash.go` persists a projection of Model/Engine/Serving, including port and excluding startup timeout | Version the payload and test history migration before changing identity |

### Release boundaries and dependency order

The PR identifiers below are planned work packages, not existing GitHub PRs. Sizes describe relative effort: S = localized change, M = multiple components, L = API/state-machine/integration change. They are not elapsed-time commitments.

| Release | PR | Deliverable | Dependencies | Size |
|---|---|---|---|---|
| R1: credible operational-safety alpha | 01 | Correct ServiceMonitor error handling | Baseline recorded | S |
| R1 | 02 | Resource-scoped metrics, queries, rules, dashboards | 01 | L |
| R1 | 03 | Replica ladder validation and complete-rung readiness | 02 | M |
| R1 | 04 | Versioned revision identity and reserved engine settings | 03 | L |
| R1 | 05 | Accurate stream metric names and token evidence | 02 | M |
| R1 | 06 | Optional monitoring resync and drift repair | 01 | M |
| R1 | 07 | Observed exposure, customer demos, README evidence | 02–06 | M |
| R2: platform integration | 08 | Authenticated Prometheus query transport | 02, 06 | M |
| R2 | 09a–c | Gateway API resources, routing state, and real integration tests | 03, 04, 07 | L |
| R2 | 10 | Existing-PVC model delivery | 04 | M |
| R2 | 11 | Persistent, revision-pinned Hub delivery | 10 | L |
| R3: second-engine release | 12 | Placement, GPU and runtime-volume contract | 04, 10 | M |
| R3 | 13 | vLLM profile and real GPU rollout demonstration | 05, 11, 12 | L |
| R4: model-quality evidence | 14a–b | External semantic gates, then evaluation Jobs | 02, 04; variant Services from 09a for Jobs | L |
| R4 | 15 | Opt-in sampled shadow evaluation | 09, 14 | L |
| R4 | 16a–c | Activation spike, bounded activator, scale-to-zero integration | 09, 11, 12 | L |

R1 is the first externally presentable milestone. It does not depend on a GPU, Gateway controller, or a semantic evaluator. R2/R3 broaden the supported deployment environments. R4 is separate because it adds new control and data paths.

Dependency map:

```text
01 → 02 → 03 → 04 ───────────→ 10 → 11 ────→ 13
│     │         │              │             ↑
│     └→ 05     └────────────────────→ 12 ───┘
└→ 06
02 + 03 + 04 + 05 + 06 → 07 → 09 → 15
02 + 06 → 08                  │     ↑
02 + 04 + 09a ──────────────→ 14 ────┘
09 + 11 + 12 ───────────────→ 16
```

Default execution is one mergeable PR at a time. Independent work can run concurrently after the shared API and metric contracts are agreed. No milestone is considered complete on the strength of fake-engine tests alone when its acceptance criterion requires a real router, storage driver, or GPU engine.

## 16. PR specifications

### PR-01 — Report ServiceMonitor apply failures correctly

**Files:** `internal/controller/observability.go`, `internal/controller/observability_test.go`.

1. Change the condition immediately following `r.Apply(...)` to `applyErr != nil`.
2. Preserve the wrapped error and `MetricsRegistered=False` verdict; ensure the reconcile path writes the verdict before returning the retryable error.
3. Add a regression test with discovery reporting ServiceMonitor present, successful earlier operations, and a client that fails specifically when applying the ServiceMonitor. Also cover the successful apply branch.

**Exit:** failure produces a False condition and retry; serving children remain present. No new API or migration is needed. This is the first implementation commit.

### PR-02 — Isolate all built-in decisions by resource identity

**Files:** `internal/metrics/names.go`; `cmd/shim/{config,help,metrics}.go`; `internal/controller/{shim,canary,autoscale}.go`; `internal/analysis/{provider,builtin}.go`; `internal/observability/{servicemonitor,prometheusrule}.go`; dashboard JSON, metric documentation, and the associated tests.

**Contract:** every canonical shim metric carries `namespace`, `model_deployment`, `model`, and `variant`. The first two identify the resource; `model` remains useful for human aggregation. Request-, prompt-, and user-derived identity labels are excluded. This follows the bounded-dimension approach in [Prometheus naming guidance](https://prometheus.io/docs/practices/naming/).

1. Add constants and shim flags for namespace and ModelDeployment name. Inject them through `shimArgs`. Standalone shim invocations must supply an identity; update fakeengine/demo fixtures and help output.
2. Extend `QueryContext` with `Namespace` and `ModelDeployment`. Centralize selector rendering with correctly escaped PromQL string literals. Canary queries select both identity labels and variant; fleet autoscaling selects the identity labels and aggregates both variants.
3. Extend the existing small substitution function with the two identity variables, retaining tolerated whitespace spellings. Document escaped values as usable inside quoted label selectors. Add separate revision/run variables later for external evaluations.
4. Update SLO aggregations to retain namespace and resource name, plus `le` where required for classic histogram quantiles. Give each rule result and alert an unambiguous identity. Do not combine resource-specific objectives into one model-only recording series.
5. Make Grafana variables namespace → ModelDeployment, and scope all workload panels accordingly. Replace hard-coded objective denominators with values generated from the resource's SLO configuration or an objective metric with the same identity.
6. Test the generated ServiceMonitor's target-label collision behavior. A scrape-time `namespace` must refer to the workload namespace; it must not replace it with the monitoring installation namespace. Verify both a direct scrape and the generated monitoring path.

**Custom query boundary:** arbitrary user-authored PromQL cannot be made isolated merely by adding template variables. Supply scoped examples and warnings for unscoped custom queries. The isolation guarantee applies to built-ins/generated rules and correctly scoped custom queries; it is not a security boundary against a user deliberately querying another tenant.

**Migration:** publish a controller/shim compatibility table and use the matching new shim image. New queries never fall back to model-only data. Upgrade with no active rollout, roll the shim producers, and wait for a complete scoped measurement window before enabling automatic analysis/scaling. Missing migration data must hold decisions rather than consume the canary error budget to an unintended abort; add an explicit metric-migration readiness condition. Test a user-pinned old shim: report incompatibility and leave a useful remediation message. Names reused after deletion also need a fresh measurement window beginning after the new object's creation.

**Tests:** new `test/chainsaw/07-metric-isolation` deploys the same model under two names in one namespace and in a second namespace. Send deliberately different error/queue loads. Verify only the selected resource fails/scales and all SLO outputs are separated. Use `promtool test rules` fixtures for aggregation and absence cases, plus real Prometheus queries in the cluster test. This PR must cover all consumers; producer-only labeling is not completion.

### PR-03 — Make each replica rung ready before evaluating it

**Files:** `api/v1alpha1/rollout_types.go`, `internal/canary/{weights,state}.go`, `internal/controller/canary.go`, validation/status helpers, samples, and tests.

**API choice:** `Replica` always uses `Split(total, desiredWeight)`. Fixed `canary.scale.replicas` and explicitly false `matchTrafficWeight` are invalid in this mode. Retain the fields for future independently weighted routing. First add controller validation and an upgrade preflight report; add admission validation once existing invalid resources are corrected. New samples never use invalid combinations.

1. Replace `CanaryAvailable bool` with observed Deployment generation, desired canary count, updated count, and ready/available count. Require the current candidate template to be observed and the whole planned rung to be ready. A ready old ReplicaSet cannot satisfy this gate.
2. Persist the rung/capacity target associated with `AvailableSince`. Clear readiness/warm-up and pending analysis evidence on rung changes, scale-target changes, or readiness loss. Preserve the existing total progress deadline so repeated readiness flaps cannot hold a rollout forever.
3. Permit a verdict only after readiness, warm-up, and a full measurement window for the current rung. Specifically, the lookback must start after warm-up ends; waiting only `initialDelay` can still query the cold-start period. Keep timing in the pure state machine and use the same due predicate before making provider calls.
4. Keep configured and ready counts distinct in status. Report quantization explicitly; never claim that rounding upward is intrinsically safer. At one total replica, hold on the stable primary with an insufficient-capacity condition. At small totals, repeated weights may legitimately map to the same count.
5. Preserve the corrected four-replica sample and update demo expectations from real status.

**Tests:** 25/50/75 gives 3/1, 2/2, 1/3; one of three ready candidates cannot advance; stale generation, lost readiness, scale changes, restart between rungs, and insufficient total capacity are covered. Property tests retain `primary + canary == planned total` for desired counts. Kubernetes rolling-update surge/termination means this is not a promise about the instantaneous number of live Pods.

**Exit:** new `08-canary-ladder` records all three ready splits and at least one complete evidence window per rung. No analysis call occurs before the rung is eligible.

### PR-04 — Preserve revision history and one authoritative engine configuration

**Files:** `internal/revision/{hash,history}.go`, `internal/controller/{children,modeldeployment_controller,canary}.go`, `internal/engine/{engine,llamacpp}.go`, and their tests. Add an ADR describing the stored revision format.

1. Introduce a versioned revision payload. Keep a decoder for today's unversioned payload and continue retrieving recorded history by its stored identity. Never recalculate an old revision's identifier with the new algorithm.
2. Add startup timeout to the workload projection; remove Service-only port from new workload identity. Add a field-by-field table identifying rollout-triggering versus policy/Service changes. Keep replicas and analysis/routing policy out of workload identity. Future storage/placement/runtime fields must declare their classification when introduced.
3. Include resolved runtime inputs that otherwise change silently, such as default engine/shim image identity, in the revision/rendering contract. Store enough effective configuration to reconstruct the prior workload after an operator upgrade. Maintain a compatibility path for old payloads that did not record these defaults.
4. Test migration from a real old `ControllerRevision` fixture. A steady workload can be adopted into the new schema only if semantic/rendered equivalence is established. Otherwise require a controlled migration rollout. An active rollout remains on its original schema until completion/abort; do not restart it because of a hash-format change.
5. Reject overrides of controller-owned engine flags and environment variables: model/source, serving host/port/alias, concurrency, worker threads, managed cache location, and metrics enablement. Inspect both short/long flags, `--flag=value`, and supported equivalent environment variables for the pinned engine. Keep unrelated sampling/tuning flags available. Errors name the conflicting setting and its supported API field.

**Tests:** startup timeout/image/placement change creates a candidate; Service port/replicas/analysis threshold change does not. Old history still restores the exact old engine/shim configuration where recorded; missing legacy data has an explicit migration outcome. Concurrency override cannot make the engine and shim disagree. Restart during migration resumes the persisted phase.

**Exit:** no history is orphaned, and an operator upgrade cannot silently reinterpret an active candidate. This is a release gate before adding more workload fields.

### PR-05 — Separate streaming chunks from reported tokens

**Files:** `cmd/shim/{sse,proxy,metrics}.go`, `internal/metrics/names.go`, `internal/analysis/builtin.go`, `api/v1alpha1/analysis_types.go`, fakeengine fixtures, dashboards, metric docs.

1. Add a clearly named content-chunk counter and an inter-chunk timing histogram for the quantities the shim can directly observe. Preserve the current first-content latency measurement and document what counts as content, including tool/reasoning-only frames.
2. Parse final protocol-supported completion usage when present, without buffering the full stream or changing the client response. Report true usage under a new unambiguous metric name, together with a count of requests that supplied usage. Do not substitute chunk counts when usage is missing.
3. Deprecate the existing chunk-derived `output_tokens_total` and `tpot_seconds` semantics. Keep legacy series temporarily with explicit deprecation documentation; migrate generated panels and built-ins to honest chunk or usage metrics. A precise token-rate gate must require sufficient usage-bearing samples. Do not leave an existing token gate silently reading the legacy approximation.
4. Add `output-chunk-rate` as the supported throughput check if needed. Keep precise time-per-token unavailable until a defensible timing/token contract exists; a final token total alone does not reveal individual token timing.

**Tests:** one SSE frame containing several tokens, multi-line frames, split UTF-8 bytes, usage-only final chunk, missing usage, client cancellation, malformed/oversized frames, and non-streaming responses. Assert byte-for-byte forwarding and incremental delivery. No billing-accuracy claim follows from this PR.

### PR-06 — Reconcile optional monitoring after installation and drift

**Files:** `internal/controller/observability.go`, `internal/controller/modeldeployment_controller.go`, `internal/discovery/discovery.go`, observability tests.

1. Add a `RequeueAfter` value to the observability result and combine it with the other timers using the smallest positive duration.
2. For enabled but absent CRDs, recheck after the existing 30-second negative TTL. For enabled healthy monitoring, use a proposed five-minute resync with bounded deterministic jitter. Disabled monitoring needs no continuing timer after owned-child cleanup completes.
3. Reconcile ServiceMonitor, PrometheusRule, and dashboard ConfigMaps on this timer. Recheck discovery and invalidate cached mappings when a previously present API disappears. Never make startup depend on optional CRDs being installed.
4. Restore only LLMCP-owned children. If the deterministic name belongs to another owner, report a conflict instead of taking it over with forced apply.

**Tests/exit:** new `09-observability-resync` starts without monitoring CRDs, installs them after the ModelDeployment is steady, then deletes/mutates generated children. Recovery occurs within the documented timer bound without editing the ModelDeployment. CRD removal reports unavailable without a hot loop. `MetricsRegistered=True` means resources were registered, not that Prometheus has scraped successfully.

### PR-07 — Expose measured traffic and publish reproducible customer evidence

**Files:** `api/v1alpha1/rollout_types.go`, `internal/controller/canary.go`, `internal/analysis/builtin.go`, `cmd/shim/{metrics,proxy}.go`, canary dashboard, `README.md`, `docs/demo.md`, `docs/assets/`.

**API proposal:** keep `desiredWeight` and `currentWeight`; add optional `observedWeight` (integer percent), `observedAt`, and `observationWindow`. Add a reason-bearing condition for insufficient/stale observations. Keep enough precision in metrics for dashboards even though status rounds to whole percentages.

The current `requests_total` counts completed requests, so its rate is a completion share and can be biased by long-running candidate streams. Add a scoped request-start counter incremented once at admission to the proxy, and derive the default observation from that counter. This describes requests reaching shim backends; Gateway rejections and requests still waiting at a future activator are outside its denominator.

1. Query primary and canary rates at the same evaluation timestamp and configured window. Use `100 * canary / (primary + canary)` only when both series are available, samples are fresh, and total traffic meets the evidence floor. A present zero canary count is valid; a missing canary series is unknown.
2. Clear observations on candidate changes and expire old values on query failure. An observation failure does not independently spend rollout failure budget. Update status at the observation cadence to avoid reconcile-driven timestamp churn.
3. Show desired/configured/observed weight together, with window and ready replica counts. Include connection-reuse experiments in Replica mode without enforcing an exact request-share assertion.
4. Capture the R1 customer scenarios in section 19. Store the sample path, git commit, image digests, environment, queries, UTC time range, and sanitized raw results beside the figures. Use genuine Grafana/status captures; label architecture illustrations as illustrations.
5. Parse all README Mermaid blocks with a pinned Mermaid parser in a docs check. Include the canary sequence regression: use `Keep stable primary and create candidate canary` instead of an unescaped semicolon separating sequence statements. Check the resulting GitHub view before publishing a release.

**Exit:** no dashboard or caption calls configured pod share measured exposure; a long-stream experiment demonstrates the distinction between starts and completions. README links to reproducible evidence and clearly labels unsupported capabilities.

### PR-08 — Add Secret-backed Prometheus authentication and TLS

**Files:** `api/v1alpha1/analysis_types.go` (`AnalysisProviderSpec`, shared with autoscaling), `internal/analysis/prometheus.go`, a new provider transport/resolver module, controller provider construction, RBAC markers, deployment overlays, docs.

**Proposed additive fields:** `authorization.credentialsSecretRef`, `basicAuth.usernameSecretRef/passwordSecretRef`, and `tlsConfig.caSecretRef/certSecretRef/keySecretRef/serverName`. References use `SecretKeySelector` in the ModelDeployment namespace. Bearer and basic authentication are mutually exclusive; certificate and key must appear together. Add a bounded allowlist of tenancy header names with Secret-backed values only if required by the integration demo. The [Prometheus Operator API](https://prometheus-operator.dev/docs/api-reference/api/) is a reference for Secret/TLS shapes; it does not configure LLMCP's separate query client automatically.

1. Build an immutable `http.Client` per resolved provider configuration with timeout, normal certificate verification, custom CA/client certificate support, and TLS 1.2 minimum. Authenticated endpoints require HTTPS except explicit local test fixtures. Reject URL userinfo and redirects to another origin; do not forward credentials to a new host.
2. Read only referenced Secrets through a direct API reader. Cache transports by provider configuration and Secret resourceVersion; resolve updates each provider cycle so rotation takes effect without restarting the manager. Close replaced idle connections. Never log Secret values, full authorization headers, or response bodies that may echo them.
3. Supply namespace-scoped Role/RoleBinding installation examples for credential access. Keep the default installation free of blanket Secret list/watch access. Missing permission or a missing Secret produces a retryable provider configuration condition, not a terminal invalid-spec verdict.
4. Use the same resolver for canary and autoscaling. Missing/invalid credentials hold decisions and report the provider error; never manufacture zero queue or success from HTTP 401/403 responses.

**Tests/exit:** HTTPS test server requiring bearer/basic/mTLS; wrong CA/hostname, expired certificate, Secret rotation/deletion, access denial, redirect, timeout, and log-redaction tests. A secured Prometheus demo succeeds with the documented limited RBAC. Cluster-level installation policy still determines which backend URLs tenants may configure.

### PR-09a–c — Decouple traffic weighting from replica allocation

**Files:** `api/v1alpha1/rollout_types.go`, `internal/canary/{state,weights}.go`, `internal/controller/{children,canary,modeldeployment_controller}.go`, `internal/naming/`, `internal/discovery/`; new `internal/routing/` renderer/observer; RBAC markers, samples, a Gateway test overlay.

**09a: API and resources.** Add `GatewayAPI` to the routing enum and a nested `gatewayAPI` configuration containing `gatewayRef` (name/optional namespace), optional `sectionName`, and hostnames. Own one same-namespace HTTPRoute and two normal ClusterIP serving Services selecting primary/canary respectively. Reference a user-managed Gateway. Keep the shared serving Service for Replica mode; in Gateway mode document that direct access bypasses weights and give it stable-only semantics. Do not use the headless metrics Service as an HTTP backend.

Use `gateway.networking.k8s.io/v1` and a pinned, tested Gateway CRD/controller pair. Envoy Gateway is the proposed first test implementation; pin its version/digest and compatibility evidence in the PR. Optional CRD discovery and periodic resync follow PR-06. Typed rendering is acceptable, but absent CRDs must not cause unconditional informer startup failure. Route and Services remain in the workload namespace; cross-namespace Gateway attachment follows listener `allowedRoutes`. Cross-namespace Service backends are excluded from this first version, so no backend ReferenceGrant is necessary. See [Gateway cross-namespace attachment](https://gateway-api.sigs.k8s.io/guides/user-guides/multiple-ns/).

**Capacity contract:** the autoscaler continues to own desired steady total. During a rollout reserve at least one stable pod and the configured candidate capacity; reject a fixed canary count that leaves no stable capacity. With total=4 and fixed canary=1, capacity stays 3/1 while weights change 25/50/75. This can deliberately overload a small candidate and must be visible in the sample. During handoff, permit only an explicit bounded surge, proposed `gatewayAPI.handoffSurgeReplicas` default 1, while accounting for child Deployment surge in the same budget. Hold if quota or the surge budget prevents safe progress. Report desired, ready, and temporary surge separately. Freeze scale-down during handoff; scale-up must preserve the handoff invariants.

**09b: durable routing state.** Introduce a routing adapter returning desired resources, observed generation/conditions, next retry, and applied weight. Reconciliation may plan a weight, apply resources, and observe acknowledgement on a later pass. It must never assume an apply call means that traffic already moved. Persist enough phase, source/destination revision, and route generation state to resume after controller restart.

Envoy Gateway provides an official [weighted HTTPRoute example](https://gateway.envoyproxy.io/docs/tasks/traffic/http-traffic-splitting/) that can seed the integration fixture.

Route gates require `Accepted=True` and `ResolvedRefs=True` for the intended parent/controller and current generation; check relevant Gateway/listener `Programmed` and supported route conditions for the pinned implementation. Treat missing/stale/False conditions as a hold. Even `Programmed` does not guarantee immediate convergence of every proxy, so require a settling/evidence window and backend readiness before analysis. [Gateway's implementer guide](https://gateway-api.sigs.k8s.io/guides/implementers-guide/) documents these condition semantics. Backend weights are relative allocations, not exact finite-sample guarantees. [Gateway traffic splitting](https://gateway-api.sigs.k8s.io/guides/user-guides/traffic-splitting/) defines their denominator.

**Promotion handoff:** do not send 100% to a fixed one-pod candidate merely to rename it.

1. Final gate passes; persist `Promoting` while retaining the stable revision as rollback history.
2. At the accepted current weights, scale candidate capacity up within the total-plus-surge budget before increasing candidate traffic. Reduce stable capacity only after its traffic decreases and in-flight streams drain. Progress in bounded steps until the candidate can carry the full load; hold on capacity failure.
3. Route 100% to ready candidate capacity. Wait for current-generation acknowledgement and the drain/settle policy. Retain the old revision's history.
4. Rebuild the primary deployment on the candidate revision. Grow it and transfer traffic from candidate to primary in capacity-safe steps, keeping the aggregate surge bounded. Both endpoints now serve the new revision.
5. After the new primary can carry the load and routing is acknowledged at primary=100/canary=0, drain and remove candidate resources and mark promotion complete.

**Rollback handoff:** while stable pods exist, direct new traffic to stable and restore its capacity before candidate cleanup. During a partially completed promotion, reconstruct stable capacity from the recorded revision before routing to it. With no ready stable endpoints, hold/restore and report degraded recovery; do not route into an empty Service. Long-running SSE streams need a bounded drain deadline and shutdown behavior, verified with the shim and gateway; changing weights does not move an established request.

**09c: integration/exit.** New `10-gateway-routing` exercises 25/50/75 with one fixed candidate, keep-alive and HTTP/2 clients, sustained SSE, absent Gateway, bad parent permissions, unresolved backends, stale conditions, route deletion, quota exhaustion, controller restarts in every handoff phase, and rollback during promotion. For distribution assertions use sufficient requests and a stated statistical tolerance, not exact equality. Record the tested streaming/idle timeout settings. Provide a TLS listener example whose certificate lifecycle and client authentication remain user-managed infrastructure.

### PR-10 — Mount models from an existing PVC

**Files:** `api/v1alpha1/modeldeployment_types.go`, `internal/controller/children.go`, `internal/engine/{engine,llamacpp}.go`, revision tests, samples; new `internal/controller/modeldelivery.go` if extraction reduces renderer complexity.

1. Implement the existing `persistentVolumeClaim` union member before inventing a new source API. Resolve its existing claim/path/readOnly fields into the engine BuildContext and a volume mount. Require a same-namespace filesystem claim; do not create, resize, own, or garbage-collect the user's PVC.
2. Define path relative to the volume root while preserving the API's absolute-path spelling: `/qwen/model.gguf` maps to `/models/qwen/model.gguf` inside the pod. Reject traversal and malformed paths. Keep model mounts read-only by default, and validate any engine-specific write requirement explicitly.
3. Distinguish invalid configuration from a claim that is missing/unbound or a file that is not ready yet. The latter remain retryable with useful conditions; do not reject a WaitForFirstConsumer claim simply because it is not bound before a pod exists.
4. Document access modes accurately: RWO permits multiple pods on one node, not general multi-node mounting; RWOP permits only one pod and conflicts with overlapping primary/canary use of the same claim. ROX/RWX availability depends on the driver. A read-only container mount is a separate setting. See [Kubernetes Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/).
5. Treat mounted model content as an immutable artifact for a revision. Add/document a content revision or digest field where the same claim/path may be reused; changing files in place otherwise bypasses the revision hash. Preserve old artifacts for rollback.

**Tests/exit:** new `11-pvc-source` serves a real GGUF from a prepopulated claim, restarts without downloads, promotes between two immutable paths, rolls back, and deletes the ModelDeployment without deleting the claim. Test missing claim/path and blocked mounts. A multi-node shared-volume claim is only advertised after testing a real compatible CSI/storage setup; single-node kind does not prove it.

### PR-11 — Persist Hub artifacts without mixing engine cache layouts

**Files:** model source types, model-delivery renderer, engine BuildContext/profiles, revision encoding, downloader image/tooling, samples and docs.

**Design choice:** add optional Hub `revision` and `cache.persistentVolumeClaim` configuration. Retain ephemeral behavior when cache is omitted. Persist immutable repository snapshots and separate the writable download/cache area from the model directory consumed by the engine. Artifact revision must participate in workload identity; production examples pin a commit or verified artifact digest.

1. Start with one download init container using a pinned `huggingface_hub` client to materialize a file for llama.cpp or a repository directory for vLLM. Resolve the requested revision, verify completion, and publish the resolved snapshot path only after success. Mount credentials through Secret environment/file references; do not put tokens in arguments.
2. Isolate cache paths by repository and revision, preserve the Hub client's locking/atomic-download behavior, and verify it on the supported shared filesystem. Test interrupted downloads and concurrent primary/canary startup. The [Hub cache design](https://huggingface.co/docs/huggingface_hub/en/guides/manage-cache) supplies the layout/locking baseline, but does not guarantee every storage driver's semantics.
3. For the existing direct llama.cpp download path, preserve `LLAMA_CACHE` behavior for `server-b10731`; the pinned [llama.cpp source](https://raw.githubusercontent.com/ggml-org/llama.cpp/b10731/common/common.cpp) and local `llamacpp.go` are the version-specific references. Do not point llama.cpp and a Python downloader at an assumed interchangeable directory. Persistent mode should pass a resolved local model path to the engine.
4. Define permissions for non-root download/runtime containers, cache capacity limits, and cleanup ownership. Cache eviction is an explicit administrative operation and must not remove artifacts referenced by active or retained rollback revisions.

**Tests/exit:** warm restart transfers no model payload; a new revision creates a distinct snapshot; interrupted concurrent downloads recover; full disk, denied token, unreachable Hub, and missing cache claim report actionable failures. Rollback uses a retained local artifact when the Hub is unreachable. Record measured warm/cold startup times without guaranteeing a fixed speedup.

### PR-12 — Add a constrained pod placement and runtime contract

**Files:** `api/v1alpha1/modeldeployment_types.go`, `internal/controller/children.go`, `internal/engine/engine.go`, revision encoding, validation and renderer tests.

Add a `placement` object containing nodeSelector, affinity, tolerations, topologySpreadConstraints, optional priorityClassName, and optional runtimeClassName. Reuse Kubernetes types with bounded validation; do not expose a raw PodSpec. These fields affect workload identity and must round-trip in revision history.

Keep GPU resources under existing engine resource requests/limits. Validate extended-resource integer quantities and coherent requests/limits; require an installed device plugin and compatible nodes. Kubernetes describes this model in [Schedule GPUs](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/). Scheduling cannot install drivers or create GPU capacity.

Extend BuildResult only as needed for named, controller-validated writable runtime directories and bounded shared memory. Engines may require `/tmp`, compilation caches, and `/dev/shm`; account for memory-backed volumes in resource examples. Mount these without relaxing the existing non-root/read-only-root security contract globally. Derive topology selectors from the specific ModelDeployment so unrelated workloads are not grouped accidentally.

**Tests/exit:** deterministic pod rendering, placement-triggered canary/rollback, CPU/GPU selectors, unschedulable conditions, and engine-specific runtime volume limits. A real GPU-node smoke test is required for GPU support claims.

### PR-13 — Add vLLM as a real second engine

**Files:** engine enum, new `internal/engine/vllm.go` and tests, registry in `internal/engine/engine.go`, model-delivery handling, docs, samples, GPU CI workflow.

1. Register a stateless profile with a build/digest-pinned image and version-specific CLI fixture. Map the resolved model directory/repository, served-model name, engine port, context length, and scheduler concurrency through authoritative fields. Reject reserved overrides as in PR-04; reject GGUF-only source combinations that the selected profile does not support.
2. Expand model delivery from a single copied file to an explicit file-or-directory artifact contract. Support a pinned Hub snapshot and a prepopulated PVC directory first; add OCI directory packaging/copying so the documented source matrix is explicit. Do not pass the existing placeholder GGUF OCI sample to vLLM.
3. Use the documented `/health` probe and retain the shim for canonical operational metrics. Scrape `/metrics` for engine diagnostics. The [vLLM online-serving documentation](https://docs.vllm.ai/en/latest/serving/online_serving/) establishes these endpoints and OpenAI-compatible serving; exact flags and runtime requirements must be pinned to the image chosen in this PR.
4. Validate the relationship between vLLM scheduler capacity and the shim's estimated queue depth. Continuous batching is not identical to llama.cpp slots; keep queue estimates labeled as estimates, and provide a measured concurrency-based autoscaling sample until the queue model is calibrated.
5. Run real inference on a GPU runner with a small licensed model and enough capacity for primary plus candidate. Keep fakeengine tests for fast state-machine coverage, but publish the real-engine result separately.

**Exit:** OpenAI chat streaming, cancellation, readiness/warm-up, canonical metrics, a good candidate promotion, an injected operational regression rollback, and offline PVC restart work on the documented GPU/image combination. Engine switch rollback restores both the old artifact format and engine. A normal laptop kind run is not accepted as GPU evidence.

### PR-14a–b — Gate promotion on attributable evaluation results

**14a: external evaluator contract.** Add a worked example to the existing custom-PromQL checks and, where necessary, bounded query context placeholders for candidate revision and evaluation run. Identify results by namespace, ModelDeployment, immutable candidate revision, dataset version, and evaluator version. Keep run-level series bounded/expired; never label individual prompts.

Supply explicit sample-count and freshness checks as well as quality thresholds. Missing, stale, wrong-revision, or zero-sample evidence is Inconclusive/Error, never Pass. A required semantic gate must not be bypassed by the operational `onInconclusive: Promote` policy; validate the configuration or separate required evaluation policy. Demonstrate a healthy but incorrect candidate rejected using a deterministic dataset. This is evidence for that dataset and policy, not a general factuality guarantee.

**14b: owned Job workflow.** Add an optional `prePromotionEvaluation` configuration with pinned evaluator image, immutable dataset reference (read-only ConfigMap or PVC), resources, timeout, minimum samples, and bounded named score thresholds using `resource.Quantity` rather than floating-point API fields. Begin with one Job per candidate/evaluator/dataset identity; larger evaluation history can become a separately scaffolded CRD later if needed.

**Files:** analysis/rollout API types, a new evaluation reconciler/helper, `internal/controller/canary.go`, revision/run identity helpers, RBAC markers, fake evaluator fixture, tests and sample.

1. Once candidate readiness is satisfied, create an owned Job targeting the candidate-only serving Service. Do not point evaluation traffic at the shared production Service. Give synthetic evaluation requests a bounded classification or separate measurement path so they do not contaminate production operational gates or observed exposure.
2. Use Kubernetes Job completion/failure/deadline as execution state. Use a small versioned JSON result in the evaluator container's termination message, containing run identity, sample count, aggregate scores, and result-schema version. Disable fallback-to-log behavior and reject oversized/malformed results. Keep the result below 2 KiB and test the rendered Pod's message allowance; Kubernetes has per-container and per-Pod truncation limits in its [termination-message documentation](https://kubernetes.io/docs/tasks/debug/debug-application/determine-reason-pod-failure/).
3. Parse results only from the expected container and owned successful Job attempt, check identity, and persist the bounded summary in ModelDeployment status before cleanup. A completed Job with no valid result is an evaluation error. Retrying must be idempotent and must not merge failed attempts into a passing sample count.
4. Disable evaluator service-account token automount unless an explicit integration requires it; the evaluator needs inference access, not permission to mutate ModelDeployments. Retain Jobs until the result is persisted, then apply a documented retention policy. Dataset/evaluator changes invalidate prior success.

**Exit:** new `12-evaluation-gate` covers pass, measured quality regression, timeout, malformed/truncated result, insufficient samples, superseded candidate, duplicate reconcile, and manager restart. Promotion waits for all required operational and evaluation gates. Synthetic evaluation traffic and result handling are explicitly tested.

### PR-15 — Sample shadow traffic into an evaluator

**Design:** use an opt-in evaluator proxy as the Gateway request-mirror destination. It receives a sampled copy, calls the candidate, scores the candidate response, and emits the PR-14 contract. Gateway request mirroring alone cannot compare answers because mirrored responses are ignored, as specified in the [request-mirroring guide](https://gateway-api.sigs.k8s.io/guides/user-guides/http-request-mirroring/).

Add sampling controls, concurrency and request-size bounds, timeout, and retention settings. Keep mirroring disabled by default. Do not claim paired stable-versus-candidate answer comparison unless an additional capture path actually supplies the stable answer. Requests carrying confidential production content require an explicit deployment-level opt-in and documented destination/retention behavior.

**Implementation:** extend routing filters and capability checks, provide a small evaluator-proxy deployment example/component, reuse candidate Services and scoped evaluation results, and keep shadow traffic separate from production rates. Do not re-execute tools or other side effects during scoring. Failures in the mirror path must not become client response failures; required evaluation evidence may still hold promotion.

**Exit:** new `13-shadow-evaluation` verifies the client receives the primary answer unchanged, mirror-disabled sends no copies, sampling stays within a stated tolerance, unsupported Gateway mirror capability is visible, bounded overload is handled, and privacy/retention settings are exercised. Shadow evidence cannot pass a different candidate revision.

### PR-16a–c — Activate a cold deployment without competing replica owners

This is the largest new subsystem. Keep `minReplicas >= 1` until the complete activation path passes the tests below.

**16a: executable integration spike.** Prototype KEDA HTTP Add-on with one ModelDeployment and test whether a pinned version can target its `/scale` contract without also scaling child Deployments or creating a competing HPA. Exercise SSE, cancellation, cold-start timeout, and route updates. Record the resource ownership diagram and a go/no-go ADR. The official [HTTP Add-on design](https://github.com/kedacore/http-add-on/blob/main/docs/design.md) describes an interceptor, external scaler, and KEDA/HPA flow; its documented Deployment-targeted path cannot simply be assumed compatible with this operator.

**Recommended fallback if the spike fails:** an LLMCP activator owns only request admission/queueing and publishes demand independently of model pods; the existing controller remains the sole replica writer. Prefer this bounded component over two controllers editing the same child Deployment. The spike is complete when it produces a reproducible result and selects one path, not when an integration is merely discussed.

**16b: activator implementation contract.** For the LLMCP path, add `cmd/activator` and a scoped activation service/configuration. Route all scale-to-zero-enabled traffic through it, including while pods are warm, so demand remains observable. Add a controller-read demand endpoint keyed by namespace, resource name and resource UID. It reports pending/in-flight requests and freshness; protect it with service identity and a NetworkPolicy. Polling stale/unavailable demand must prevent scale-down. The activator never writes `/scale` itself.

Bound queued request count, total buffered bytes, request size, and maximum activation wait. On overflow return 429 with Retry-After; on activation deadline return a documented 503. Propagate cancellation and do not replay a request after forwarding any response bytes. Stream responses incrementally once ready. Activator shutdown drains within a configured deadline; an in-memory queue does not provide durable delivery if the process crashes, and the API/docs must state that limit.

**16c: controller integration.** Add opt-in activation policy and durable Dormant/Activating/Active/Draining status. Activate only on fresh external demand, retain a readiness deadline, and enter zero only after the idle window with no queued or in-flight requests. Pin a nonzero stable floor during canary and promotion, and ignore GitOps replica writes that would violate an active handoff only according to the documented existing ownership policy. Do not attempt a zero-pod canary. Account for cold-start SLOs separately from warm inference TTFT.

**Files:** autoscaling/serving API types, `internal/autoscale/`, controller scaling/routing helpers, activator or selected integration manifests, RBAC/service identity configuration, metrics, dedicated tests and demo.

**Exit:** new `14-scale-zero` demonstrates idle → zero → incoming request → startup → successful streamed response → idle. Cover concurrent cold requests, cancellation, quota/image/PVC failure, activator restart, stale demand, controller restart, no duplicate execution, and a long stream preventing scale-down. Record the chosen integration's pinned versions and capacity limits. Passing ordinary zero-replica schema validation is not completion.

## 17. Upgrade, API, and release execution rules

1. **Establish the baseline per PR.** Record the starting commit, current changes, supported Kubernetes version, tool versions, and feature fixtures. This research turn does not certify fresh test results or a running cluster.
2. **Make APIs additive first.** Use existing `v1alpha1` fields where meaningful. Do not rename `currentWeight`, delete old revision decoders, or enable new routing/activation behavior by default. Add preflight detection before enforcing new validation against stored resources.
3. **Regenerate from source.** Edit API comments/markers and controller RBAC markers, then run `make manifests generate api-docs`. Never hand-edit generated CRDs, RBAC, DeepCopy files, or `PROJECT`. If a later design adds a CRD/webhook, scaffold it with Kubebuilder as required by `AGENTS.md`.
4. **Exercise upgrades as state transitions.** At minimum test: steady legacy workload; an active canary; sticky failed revision; old pinned shim; optional integration absent; and operator rollback. Snapshot the CR, status, ControllerRevisions and rendered workloads before/after. Prove history can still be decoded and old workloads restored.
5. **Separate rollback compatibility from application rollback.** An older controller will not understand Gateway/evaluation/activation configuration. Disable/drain the new feature and convert configuration through a documented downgrade procedure before downgrading the controller. Do not promise unrestricted downgrade after enabling new API values.
6. **Pin deployment dependencies when their PR lands.** Record image digests, model hashes, chart/CRD versions, driver requirements, and architecture. Research references describe designs; they do not substitute for a tested version matrix. Avoid `latest` in reproducible demos.
7. **Require install/uninstall ownership checks.** ModelDeployment deletion removes owned serving/routing/evaluation resources while preserving user Gateways, Secrets, PVCs, and model artifacts. Long streams get the documented bounded shutdown behavior.

### Verification commands

These are commands for the implementation PRs. `make verify` and R1 suites 07–09
exist today; suite names 10–14 remain planned directories to create in their
corresponding PRs.

For Go/API changes:

```bash
make manifests generate api-docs
make lint-fix
make verify
git diff --check
```

`make verify` includes unit/envtest, generated API-doc checks, lint and Chainsaw schema validation. Add focused race tests when changing concurrent proxy/activator or provider transport state:

```bash
go test -race ./cmd/shim ./internal/analysis
```

For a dedicated kind environment, use an isolated kubeconfig and inspect available Docker capacity first. The registry script uses a shared local registry by default; record that dependency and do not remove unrelated clusters/registries.

```bash
docker info
kind get clusters
export KUBECONFIG="$(mktemp -t llmcp-plan-kubeconfig)"
export CLUSTER_NAME=llmcp-plan-e2e
make e2e-up
test "$(kubectl config current-context)" = "kind-llmcp-plan-e2e"
make loadgen-image
make monitoring-install
make e2e-chainsaw
```

After adding the R1 suites:

```bash
make e2e-chainsaw SUITE=07-metric-isolation
make e2e-chainsaw SUITE=08-canary-ladder
make e2e-observability-resync  # dedicated, monitoring-free *observability-resync* Kind cluster only
```

The absent-CRD test needs a separate fresh cluster or dedicated CI job before monitoring is installed; it must not delete shared CRDs from the general test cluster. Likewise, install the pinned Gateway and storage prerequisites in dedicated jobs before running their suites. Provide those installation manifests/scripts in PR-09/10 rather than leaving undocumented manual setup.

### CI lanes and completion evidence

| Lane | Environment | Must prove |
|---|---|---|
| Fast validation | Existing unit/envtest CI | Pure decisions, query isolation, admission rules, deterministic rendering, generated docs |
| Operational integration | Isolated kind + Prometheus + fakeengine | Actual metric ingestion, ladder progression, monitoring recovery, upgrades |
| Routing integration | Isolated kind + pinned Gateway controller | Real weighted traffic, route acknowledgements, streaming drain and restart safety |
| Artifact integration | Real GGUF + PVC/cache; shared-storage job where available | Cold/warm delivery, permission/access-mode behavior, offline rollback |
| GPU engine | Documented GPU runner + pinned vLLM/model | Real second-engine serving and operational canary behavior |
| Evaluation/activation | Dedicated cluster + evaluator/activator | Required evidence, cold demand, bounded wait and failure outcomes |
| Documentation evidence | Outputs of the relevant integration lane | Rendered diagrams, real captures, captions tied to the exact tested configuration |

Each PR attaches commands, results, and limitations for its required lane. An unavailable GPU/storage runner leaves that capability unverified and the release claim pending; it does not invalidate unrelated completed R1 work.

## 18. Recommended decisions and remaining experiments

The implementation program uses the following defaults so work can start after review without another architecture round:

| Decision | Recommended choice | Experiment or constraint |
|---|---|---|
| Replica routing | Weight-derived replicas; no fixed override | Migration preflight before admission enforcement |
| Metric identity | Direct namespace/resource labels on shim; no unscoped fallback | Upgrade tests and scrape-label collision tests |
| Exposure observation | Request-start share with timestamp/window; preserve currentWeight | Existing completion share remains separately understandable |
| Gateway ownership | Own route and backend Services; reference user Gateway | Verify one pinned controller and handoff surge/drain limits |
| Revision format | Versioned payload with legacy decoder and effective runtime inputs | Existing-history upgrade fixture is mandatory |
| Persistent artifacts | Existing user PVC; immutable snapshot/path; explicit downloader | Concurrent downloads and shared-driver behavior must be measured |
| Second engine | vLLM behind the same shim | Real GPU runner, model-directory support and queue-model calibration |
| Evaluation result | External metrics first; bounded Job termination result next | Truncation, stale-run rejection and evidence completeness tests |
| Scale to zero | Spike KEDA compatibility; otherwise demand-only LLMCP activator | Exactly one replica writer; bounded cold-start behavior |

The unresolved items are bounded experiments with exit criteria in their PRs, not assumed capabilities. R1 implementation can proceed without choosing a cloud GPU vendor, external Gateway installation, or semantic scoring model.

## 19. Customer demonstrations and final acceptance

| Customer scenario | Reproducible experiment | Evidence and honest claim | Required release |
|---|---|---|---|
| Internal platform serves the same model for two teams | Same model under distinct namespace/name identities; degrade only one candidate | Only that deployment's rollout/scaling/SLOs react; metric isolation, not full tenant security | R1 |
| Customer-support assistant upgrade stays healthy but slows down | Start stable, inject candidate TTFT delay, maintain load | TTFT comparison, failed gates, stable restoration and sticky failed revision captured from status/Prometheus | R1 |
| Inference service handles a traffic burst | Increase offered load then reduce it | Queue/concurrency, desired/ready replicas and user latency on one timeline; state resource limits | R1 |
| Platform team installs monitoring after serving starts | Create ModelDeployment first, install CRDs/operator later | Registration and metric recovery without editing the workload spec | R1 |
| Platform needs traffic percentages independent of GPU count | Fixed one-pod candidate behind weighted Gateway | Configured and measured 25/50/75 shares, statistical tolerance, and safe handoff; expose capacity limits | R2 |
| Restricted-network deployment reuses model storage | Prepopulate PVC/cache, restart with Hub unavailable | Model payload is reused, serving recovers, old revision remains recoverable | R2 |
| GPU platform deploys vLLM under the same release policy | Pinned vLLM/model/GPU configuration; introduce an operational regression | Same canonical gates work with a second actual engine | R3 |
| Domain assistant produces fast but unacceptable answers | Fixed dataset/evaluator; candidate fails configured quality threshold | Promotion held/rejected for that versioned dataset; no general hallucination-free claim | R4 |
| Low-traffic service saves idle serving capacity | Idle to zero, then issue simultaneous streaming requests | Activation wait, bounded queue, response/error outcomes, and long-stream drain behavior | R4 |

For each published demonstration, commit the configuration and capture script, save sanitized machine-readable measurements, and build figures from that run. A complete release has install instructions, a supported-version/source matrix, upgrade and rollback instructions, and captions that explain what the figures actually measure.

**First execution batch after review:** PR-01 → PR-02 → PR-03, then finish PR-04–07 for R1. This delivers a presentable, defensible operational-safety alpha before committing to the larger Gateway, GPU, evaluation, and activation work.
