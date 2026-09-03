# Mini LLM Inference Control Plane

A Go Kubernetes operator that deploys an LLM inference server, health-checks it,
autoscales it, runs **canary releases gated on metric analysis**, and
**automatically rolls back on regression** — with Prometheus/Grafana
observability, running locally on kind against a 0.6B CPU model.

**The thesis:** apply the same control law the GPU-scale ecosystem converged on
— scale on **queue depth**, gate promotion on **TTFT and error rate**, never on
CPU utilization — to a tiny CPU model on a laptop, and add the one thing the
closest prior art (KubeAI) lacks: **canary with metric-driven automated
rollback**, with model version and engine version independently canary-able.

```yaml
apiVersion: inference.llmcp.io/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen3
spec:
  replicas: 4
  model:
    name: qwen3-0.6b
    source:
      image: { image: localhost:5001/llmcp-model:qwen3-0.6b-q4km, path: /weights/model.gguf }
  engine:
    type: llamacpp
    resources:
      limits: { cpu: "2", memory: 2Gi }     # -t is derived from this
  rollout:
    type: Canary
    canary:
      stepWeights: [25, 50, 75]
      analysis:
        metrics:
          - name: ttft-vs-primary
            builtin: ttft-p95
            compareToPrimary: true          # a ratio, not an absolute threshold
            thresholdRange: { max: "1500m" }
  autoscaling:
    mode: Builtin
    maxReplicas: 6
    metric: queue-depth                     # not CPU
```

---

## Why a controller and not a Deployment

Kubernetes already does rolling updates correctly, and this operator delegates
them to the Deployment controller unchanged. What a Deployment cannot do is
notice that the new pods are **healthy but worse** — passing every readiness
probe, returning 200 to every request, and serving three times the latency — and
undo itself.

That is the gap this project fills, and everything else exists to make it
possible:

| Piece | Exists because |
|---|---|
| **`cmd/shim`**, a native sidecar | llama.cpp exposes **gauges only**. No histograms, no TTFT, no status-code counter — so p95 latency and success rate are not awkward to compute, they are *impossible*. A canary would have nothing to gate on. |
| **Four-valued verdicts** | `Pass \| Fail \| Inconclusive \| Error` with **disjoint counters**, so a Prometheus outage yields `Error` and can never cause a production rollback. |
| **`minRequestRate`** | With no traffic, error rate is 0 and every percentile is absent — so an ungated canary passes every check having served *nothing*. |
| **Queue depth** | An inference server saturates its decode slots long before its CPU. By the time utilization trips a 70% target, tail latency has been bad for minutes. |
| **A pure state machine** | `canary.Next` and `autoscale.Recommend` take a value and return a value. A five-step rollout with a 60s warm-up replays in **microseconds** on a fake clock. |

---

## Quick start

```bash
make kind-up                 # 3-node kind cluster + local registry
make model-image-real        # ~400 MB of Qwen3 weights, checksum-verified
make dev-images dev-deploy   # controller, shim, stub engine, load generator
make monitoring-install      # kube-prometheus-stack, slimmed

kubectl apply -f config/samples/qwen3_llamacpp.yaml
kubectl wait modeldeployment/qwen3 --for=condition=Ready --timeout=300s

make load MD=qwen3 &         # in-cluster streaming load
make grafana                 # three dashboards, imported by nobody
```

**→ [`docs/demo.md`](docs/demo.md) is the 10-minute walkthrough**, including the
canary rolling itself back and the proof that a monitoring outage does not
trigger one.

---

## Architecture

```
        ┌─────────────────────────── ModelDeployment (CRD) ───────────────────────────┐
        │  spec.replicas is the TOTAL across variants — the /scale subresource target  │
        └──────────────────────────────────────┬──────────────────────────────────────┘
                                               │
              autoscale.Recommend ──► desiredTotal ──► canary.Split ──► (primary, canary)
                     ▲                                                          │
                     │ queue depth                                              ▼
                     │                                    ┌──────────────┬──────────────┐
                     │                                    │  <md>-primary│  <md>-canary │
                     │                                    │  (stable rev)│ (target rev) │
                     │                                    └──────┬───────┴──────┬───────┘
                     │                                           │              │
                     │                          ┌────────────────▼──────────────▼──────┐
                     │                          │  pod: [shim] ──► [engine]            │
                     │                          │  native sidecar, starts first,       │
                     │                          │  terminates last, owns readiness     │
                     │                          └────────────────┬─────────────────────┘
                     │                                           │ llmcp_*
                     └───────────── Prometheus ◄──────────────────┘
                                        │
                     canary.Next ◄──────┘  analysis.Evaluate → Pass|Fail|Inconclusive|Error
```

Five decisions carry the design — each has an ADR:

- [**0001**](docs/adr/0001-spike-closed-loop.md) — the feasibility spike, with real numbers
- [**0002**](docs/adr/0002-model-weight-delivery.md) — weights as an OCI image, not `ImageVolume`
- [**0003**](docs/adr/0003-metrics-shim.md) — the shim, and the three things that fail *silently*
- [**0004**](docs/adr/0004-canary-analysis.md) — four verdicts, disjoint counters, a pure core
- [**0005**](docs/adr/0005-autoscaling.md) — queue depth, `/scale`, and no `mode: HPA`
- [**0006**](docs/adr/0006-slo-and-dashboards.md) — TTFT as the SLI, and why not duration
- [**0007**](docs/adr/0007-hardening-and-ci.md) — lint rules that encode the lessons

Also: [`docs/api.md`](docs/api.md) (generated, drift-checked) ·
[`docs/slo.md`](docs/slo.md) (the runbook every alert links to) ·
[`research.md`](research.md) (the verified stack, with the rejected options)

---

## Testing

Three tiers, with explicit jobs. The distinction matters because **envtest runs
no controllers and no kubelet** — a Deployment created there never creates a
ReplicaSet, never schedules a pod, and its status stays at zero forever.

| Tier | Has | Proves | Runtime |
|---|---|---|---|
| unit | nothing | `canary.Next`, `autoscale.Recommend`, `analysis.Evaluate`, every generated query | ms |
| envtest | a real API server + etcd | CEL, defaulting, `/scale`, SSA idempotency, conditions, GC | ~20 s |
| [Chainsaw](test/chainsaw/) | a real cluster | pods actually start, the HPA controller acts, traffic reaches the shim | minutes |

```bash
make verify          # everything that needs no cluster
make e2e-up          # cluster + operator + images
make e2e-chainsaw    # the six suites
```

Determinism comes from three mechanisms, not from retries:

1. **An injected `clock.PassiveClock`**, with `time.Now()` banned by lint outside
   `main`. An entire five-step canary runs in microseconds via `fakeClock.Step()`.
2. **A scripted `MetricProvider`** — `"pass, pass, fail, fail"` asserts that
   rollback fires on *exactly* the second failure, which is impossible against a
   live Prometheus.
3. **`RequeueAfter` as a return value**, so a test asserts on rollout timing
   instead of measuring how long the controller slept.

---

## Status

Sprints 0–7 complete. The vLLM engine profile (Sprint 8, committed stretch) is
the remaining work — it proves `EngineSpec` is a real abstraction rather than a
fiction, since the canary demo should run unchanged across an engine swap.

**Not yet proven:** the Chainsaw suites are written and schema-validated
(`make chainsaw-lint`, part of `make verify` and of CI) but have not been
executed against a live cluster — Docker was unavailable. Everything that runs
without a cluster is green, and is what every other claim here rests on.

---

## License

Copyright 2026. Licensed under the Apache License, Version 2.0.
