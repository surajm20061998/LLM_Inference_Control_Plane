# Demo: catch a healthy-but-slower model release

This walkthrough demonstrates the business case for LLMCP: a candidate remains Kubernetes-ready and returns successful responses, but its time to first token regresses. Prometheus supplies the evidence, and the controller restores the last known-good revision.

The main path uses the deterministic fake engine so the regression is repeatable. It still exercises the real `ModelDeployment` API, llama.cpp engine profile, generated Deployments and Services, native metrics shim, Prometheus queries, state machine, rollback, and Kubernetes Events.

> [!NOTE]
> This is an **operational-quality** demonstration. It does not evaluate whether the model's answer is factual, safe, or useful.

![A live OpenAI-compatible streaming request completing through the LLMCP shim while HTTP and TTFT metrics are recorded](assets/runtime-request.png)

The figure above was reproduced from the repository's deterministic engine and shim binaries. The steps below add the Kubernetes controller, rollout state, and Prometheus decision loop around that same request path.

## What the demo proves

- A single resource creates a working OpenAI-compatible inference endpoint.
- Every request passes through the shim that measures TTFT, duration, status, concurrency, and queue depth.
- A candidate with successful readiness checks and HTTP responses can still fail the rollout on latency.
- Low traffic, a measured failure, and an unavailable metrics provider are different outcomes.
- Rollback restores the stable pod template and records the rejected revision so it is not retried forever.

## 1. Prepare the local platform

Allow 10–20 minutes on a cold machine because the monitoring chart and container images may need to download.

```bash
make kind-up
make dev-deploy
make loadgen-image
make monitoring-install
```

This creates a three-node kind cluster and local registry, deploys the controller, and installs the pinned kube-prometheus-stack configuration.

Verify the platform:

```bash
kubectl get nodes
kubectl -n llmcp-system rollout status deploy/llmcp-controller-manager
kubectl -n monitoring get pods
```

## 2. Establish the known-good revision

Use the rollback fixture directly rather than running Chainsaw. The fixture remains in the default namespace so its status, metrics, and Events are available for inspection after the rollout.

```bash
kubectl apply -f test/chainsaw/04-canary-rollback/modeldeployment.yaml
kubectl wait modeldeployment/rollback-demo \
  --for=condition=Ready --timeout=300s

kubectl get modeldeployment rollback-demo
kubectl get deployment,pod,service \
  -l app.kubernetes.io/instance=rollback-demo -o wide
```

The initial deployment becomes the last known-good revision. The operator cannot canary the first revision because there is no stable baseline to compare against.

Send one streaming request:

```bash
kubectl run llmcp-client --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 -- \
  curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"stub-model","stream":true,"messages":[{"role":"user","content":"Hello from the LLMCP demo"}]}' \
  http://rollback-demo.default.svc:8080/v1/chat/completions
```

## 3. Generate comparable traffic

```bash
kubectl apply -f test/chainsaw/04-canary-rollback/loadgen.yaml
kubectl get job rollback-load -w
```

The load generator disables HTTP keep-alive. Replica routing is connection-level, so reusing a small set of long-lived connections can pin callers to individual pods and make observed request distribution diverge from the configured pod share.

Confirm Prometheus registration from status:

```bash
kubectl get modeldeployment rollback-demo \
  -o jsonpath='{.status.conditions[?(@.type=="MetricsRegistered")].message}{"\n"}'
```

## 4. Inject a healthy latency regression

The stable fake engine emits its first token after 60 ms. Change the candidate to 400 ms while leaving readiness and the HTTP success path intact:

```bash
kubectl patch modeldeployment rollback-demo --type=merge -p '
{"spec":{"engine":{"env":[
  {"name":"LLMCP_FAKE_TTFT_MS","value":"400"},
  {"name":"LLMCP_FAKE_TOKENS","value":"24"}
]}}}'
```

Watch the control-plane state:

```bash
kubectl get modeldeployment rollback-demo -w -o custom-columns=\
'PHASE:.status.phase,STEP:.status.canary.step,WANT:.status.canary.desiredWeight,CONFIGURED:.status.canary.currentWeight,FAIL:.status.canary.failedChecks,ERROR:.status.canary.consecutiveErrors'
```

At the first rung, the four-pod fixture configures three primary replicas and one canary replica. `currentWeight` is therefore 25—the configured replica share, not a measured request percentage.

## 5. Observe the automatic rollback

The fixture uses a short demonstration window and rolls back after two failed analysis rounds. Wait for the decision:

```bash
kubectl wait modeldeployment/rollback-demo \
  --for=condition=CanaryHealthy=False --timeout=8m

kubectl get events \
  --field-selector reason=CanaryRolledBack \
  --sort-by=.lastTimestamp
```

Prove that the stable configuration is still serving and the candidate Deployment is gone:

```bash
kubectl get deployment rollback-demo-primary \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="engine")].env}{"\n"}'

kubectl get deployment rollback-demo-canary
kubectl get modeldeployment rollback-demo \
  -o jsonpath='{.status.canary.failedRevision}{"\n"}'
```

The first command contains the stable 60 ms setting. The second returns `NotFound`. The final command returns a revision hash: the user's spec still asks for the slow configuration, but the controller records its rejection instead of starting the same failing rollout on every reconcile.

## 6. Inspect the dashboards

Start a port-forward and open <http://localhost:3000> with `admin` / `admin`:

```bash
make grafana
```

In the **LLMCP — Canary** dashboard:

1. choose `stub-model`;
2. use a time range covering the last 15 minutes;
3. set refresh to 5 seconds;
4. compare **Traffic weight over time** and **TTFT p95 — canary against primary**;
5. find the rollback annotation at the point where the candidate diverged.

The removed canary series remains in Prometheus long enough to inspect immediately after rollback.

The other installed dashboards are:

- **LLMCP — Control Plane** for reconciliation, phase, and operator activity;
- **LLMCP — Inference SLO** for TTFT/availability objectives, burn rates, and no-traffic detection.

## 7. Understand metrics-provider failure behavior

Prometheus errors are `Error`, not `Fail`. They do not spend the budget used to declare a candidate operationally worse. The controller initially holds and retries; after `consecutiveErrorLimit`, it aborts the rollout back to the stable revision rather than leaving an unmeasurable candidate active indefinitely.

That distinction is why the implementation keeps separate failed, inconclusive, and consecutive-error counters.

## 8. Optional: serve a real Qwen3 model

The deterministic engine proves the control-plane behavior. This separate path proves real inference through llama.cpp and the same shim.

```bash
make model-image-real
kubectl apply -f config/samples/qwen3_llamacpp.yaml
kubectl wait modeldeployment/qwen3 --for=condition=Ready --timeout=300s
```

Send a real streamed completion:

```bash
kubectl run qwen-client --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 -- \
  curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-0.6b","stream":true,"messages":[{"role":"user","content":"Explain a Kubernetes operator in one sentence."}]}' \
  http://qwen3.default.svc:8080/v1/chat/completions
```

The sample requests up to six CPU cores and six GiB of memory across three replicas. Do not combine this real-model run with every other sample on a small Docker Desktop VM.

## 9. Autoscaling demonstration

After removing the previous workloads, apply [`config/samples/autoscaling_qwen3.yaml`](../config/samples/autoscaling_qwen3.yaml) and generate load:

```bash
kubectl delete modeldeployment rollback-demo qwen3 --ignore-not-found
kubectl apply -f config/samples/autoscaling_qwen3.yaml
kubectl wait modeldeployment/qwen3-auto --for=condition=Ready --timeout=300s

make load MD=qwen3-auto CONCURRENCY=24 DURATION=10m
```

Watch queue pressure and desired capacity:

```bash
kubectl get modeldeployment qwen3-auto -w -o custom-columns=\
'DESIRED:.spec.replicas,READY:.status.readyReplicas,PER_POD:.status.autoscaling.currentMetricPerPod,MESSAGE:.status.autoscaling.message'
```

The controller can add capacity immediately but uses a longer scale-down stabilization window because removing a model pod creates future cold-start cost.

## Cleanup

```bash
kubectl delete modeldeployment --all
kubectl delete job rollback-load --ignore-not-found
make monitoring-uninstall
make kind-down
```

## Troubleshooting

| Symptom | Check |
|---|---|
| `Ready` never becomes `True` | `kubectl describe modeldeployment <name>` and the `ModelReady` condition. |
| Grafana panels are empty | `MetricsRegistered`, then Prometheus **Status → Targets**. |
| Canary never advances | `status.canary.checks[*].verdict`; `Inconclusive` commonly means insufficient traffic. |
| Canary aborts without a failed metric | Inspect `consecutiveErrors` and Prometheus availability. |
| The load Job cannot pull its image | Run `make loadgen-image`; it is intentionally separate from `dev-images`. |
| HPA reports `<unknown>` CPU | The serving pod needs a CPU request because utilization is a percentage of the request. |
