# Chainsaw suites

These run against a **real cluster** and are the tier that the unit and envtest
tiers cannot reach.

The distinction is worth being precise about, because Sprints 3 and 4 shipped
with their exit criteria proven only at the envtest tier and that is a gap
rather than a preference:

| Tier | Has | Proves |
|---|---|---|
| unit | nothing | pure logic: `canary.Next`, `autoscale.Recommend`, `analysis.Evaluate` |
| envtest | a real API server and etcd | CEL, defaulting, `/scale` round-trips, SSA idempotency, conditions |
| **Chainsaw** | **a real cluster** | **that pods actually start, that the built-in Deployment/HPA controllers act, that traffic reaches the shim** |

envtest runs **no controllers and no kubelet**. A Deployment created there never
creates a ReplicaSet, never schedules a pod, and its `.status` stays at the zero
value forever — which is why `test/helpers.MarkDeploymentAvailable` exists. So
envtest can prove that this operator publishes a correct `/scale` subresource,
but only a real cluster can prove that a stock HorizontalPodAutoscaler *reads*
it and writes back.

## Running

```bash
make e2e-up                           # cluster + stub images + operator
make loadgen-image monitoring-install # prerequisites for suites 03/04/07/08
make e2e-chainsaw                     # all non-destructive suites
make e2e-chainsaw SUITE=05-scale-subresource
```

Suite `09-observability-resync` deliberately creates and removes the canonical
Prometheus Operator CRDs. It is excluded from the default set and refuses to run
unless the current context is a disposable Kind cluster whose name contains
`observability-resync`. Run it only before installing a monitoring stack:

```bash
CLUSTER_NAME=llmcp-observability-resync make kind-up
make dev-deploy
make e2e-observability-resync
```

Every suite uses the **deterministic stub engine**, not llama.cpp. A real model
takes minutes to load and its latency varies by orders of magnitude between
runs, so assertions written against it would be flaky by construction. The stub
produces exactly the latency, token count and failure pattern it is told to.

## Rules

- **Assert on status fields and events, never on timing.** No `sleep`. Chainsaw
  polls until an assertion holds or the step times out, which is the only form
  of waiting that does not encode one machine's speed into the test.
- **One namespace per suite**, created and torn down by Chainsaw, so suites can
  run independently. Suite 07 additionally owns and removes one labeled peer
  namespace. Suite 09 is isolated at the cluster level because CRDs are
  cluster-scoped.
