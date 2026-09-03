# The 10-minute demo

The thesis, in one sentence: **apply the control law the GPU-scale ecosystem
converged on — scale on queue depth, gate promotion on TTFT and error rate,
never on CPU — to a tiny CPU model on a laptop, and add the one thing the
closest prior art lacks: canary with metric-driven automated rollback.**

This script demonstrates that in ten minutes. Every command is copy-pasteable
and every assertion it makes is one the test suite also makes.

---

## 0. Setup (before the clock starts — allow ~20 minutes)

```bash
make kind-up                 # 3-node kind cluster + a local registry
make model-image-real        # downloads ~400 MB of Qwen3 weights, verifies the checksum
make dev-images dev-deploy   # controller, shim, stub engine, load generator
make monitoring-install      # kube-prometheus-stack, slimmed
```

Two port-forwards, left running:

```bash
make grafana      # http://localhost:3000  admin / admin
make prometheus   # http://localhost:9091
```

Grafana should already show three dashboards under **LLM Inference**, imported
by nobody. They are `//go:embed`ed into the operator binary and published as
ConfigMaps the Grafana sidecar discovers by label.

---

## 1. A model, deployed (1 min)

```bash
kubectl apply -f config/samples/qwen3_llamacpp.yaml
kubectl wait modeldeployment/qwen3 --for=condition=Ready --timeout=300s
kubectl get modeldeployment qwen3
```

```
NAME    ENGINE     MODEL        DESIRED   READY   PHASE       WEIGHT   AGE
qwen3   llamacpp   qwen3-0.6b   3         3       Available            2m
```

**Worth pointing out:** the thread count.

```bash
kubectl get deploy qwen3-primary -o jsonpath='{..args}' | tr ' ' '\n' | grep -A1 '^-t$'
```

llama.cpp reads the *host's* `/proc` and is cgroup-unaware, so left alone every
pod would start one worker per host core and three replicas would contend for
eight cores. The operator derives `-t` from `resources.limits.cpu`. Without it,
latency percentiles become scheduler noise — and the canary analysis two steps
from here would then be comparing noise to noise.

Serve a real completion:

```bash
kubectl run curl --rm -it --restart=Never --image=curlimages/curl:8.11.1 -- \
  curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-0.6b","stream":true,"messages":[{"role":"user","content":"Explain a Kubernetes operator in one sentence."}]}' \
  http://qwen3.default.svc:8080/v1/chat/completions
```

---

## 2. Metrics that could not exist otherwise (1 min)

```bash
kubectl get modeldeployment qwen3 -o jsonpath='{.status.conditions[?(@.type=="MetricsRegistered")].message}'
```

llama.cpp's `/metrics` exposes **gauges only** — no histograms, no request
duration, no time-to-first-token, no status-code counter. p95 latency and
success rate are therefore not awkward to compute from it, they are
*impossible*. So the operator injects `llmcp-shim` as a native sidecar that
reverse-proxies `/v1/*` and emits the missing signals.

```bash
kubectl get deploy qwen3-primary \
  -o jsonpath='{.spec.template.spec.initContainers[*].name}'
# model-init shim
```

`shim` is in `initContainers` with `restartPolicy: Always` — a **native
sidecar**. It starts before the engine, terminates after it, and its readiness
counts toward the pod's. So there is no window in which traffic is served
unmeasured at either end of a pod's life.

Start load and prove the measurement is real:

```bash
make load MD=qwen3 DURATION=10m CONCURRENCY=8 &
sleep 90
make verify-ttft
```

```
  request rate         6.41 req/s
  ttft_p95            0.412 s
  duration_p95        1.930 s
  ratio               0.213  (must be < 0.50)

PASS: time to first token is a genuine fraction of total duration.
```

**Why that assertion and not "TTFT looks plausible":** three separate mistakes
in a streaming proxy — a missing `FlushInterval: -1`, gzip left enabled on the
upstream hop, or timing the first response *byte* rather than the first
content-bearing SSE frame — all produce a TTFT metric that still exists, still
moves under load, and has silently become a duplicate of the duration metric.
Nothing errors. The ratio collapsing to 1.0 is the only symptom.

---

## 3. The centrepiece: a canary that rolls itself back (4 min)

```bash
kubectl apply -f config/samples/canary_qwen3.yaml
kubectl wait modeldeployment/qwen3-canary --for=condition=Ready --timeout=300s
make load MD=qwen3-canary DURATION=15m CONCURRENCY=8 &
```

Open the **LLMCP — Canary** dashboard. Then trigger a rollout:

```bash
kubectl patch modeldeployment qwen3-canary --type=merge \
  -p '{"spec":{"engine":{"contextSize":8192}}}'

kubectl get modeldeployment qwen3-canary -w -o custom-columns=\
'PHASE:.status.phase,STEP:.status.canary.step,WANT:.status.canary.desiredWeight,GOT:.status.canary.currentWeight,FAIL:.status.canary.failedChecks'
```

```
PHASE       STEP   WANT   GOT   FAIL
Canarying   0      25     25    0
Canarying   1      50     50    0
Canarying   2      75     75    0
Promoting   2      100    100   0
Available   0      0      0     0
```

**Point at `WANT` vs `GOT`.** They agree here because the sample uses four
replicas and the ladder starts at 25%. At three replicas a requested 25% would
realise as 33%, and the operator would report both numbers and fire a
`WeightQuantized` event. Traffic is split by pod count, so the achievable
weights are multiples of 1/replicas — publishing only the requested figure would
let you believe the blast radius is smaller than it is.

### Now break it

```bash
kubectl patch modeldeployment qwen3-canary --type=merge -p '
{"spec":{"engine":{"env":[{"name":"LLMCP_FAKE_TTFT_MS","value":"1200"}]}}}'
```

> Against the real llama.cpp engine, use a CPU limit instead — the spike used
> `500m` against the primary's `2`, which starves exactly the prompt-processing
> work that TTFT measures:
> `kubectl patch ... -p '{"spec":{"engine":{"resources":{"limits":{"cpu":"500m"}}}}}'`

```bash
kubectl wait modeldeployment/qwen3-canary \
  --for=condition=CanaryHealthy=False --timeout=300s

kubectl get events --field-selector reason=CanaryRolledBack
```

```
LAST SEEN   TYPE      REASON             OBJECT                              MESSAGE
12s         Warning   CanaryRolledBack   modeldeployment/qwen3-canary        Rolling back: 3 failed checks reached the threshold of 3
```

On the canary dashboard, a **red annotation line** lands on the TTFT graph at
the instant the controller decided. That is the money shot: the two variants'
p95 diverging, and the vertical line where the divergence became a decision.

**The thing to say out loud:** those canary pods were *healthy*. Every readiness
probe passed. Every request returned 200. They were simply three times slower,
and Kubernetes has no opinion about that whatsoever — a Deployment would have
rolled this out and considered the job done.

### And it stays rolled back

```bash
kubectl get modeldeployment qwen3-canary -o jsonpath='{.status.canary.failedRevision}'
```

The spec still names the slow configuration. Without recording the rejection the
controller would start an identical canary on the very next reconcile — an
infinite loop of failing rollouts, each costing a full analysis window. A
rollback has to *stick* until a human changes the spec.

---

## 4. The safety property nobody demos (1 min)

```bash
kubectl patch modeldeployment qwen3-canary --type=merge \
  -p '{"spec":{"engine":{"contextSize":4096}}}'   # start a fresh, healthy canary
sleep 60

# Now take the monitoring away.
kubectl -n monitoring scale sts prometheus-kps-kube-prometheus-stack-prometheus --replicas=0
sleep 90

kubectl get modeldeployment qwen3-canary \
  -o jsonpath='{.status.canary.checks[*].verdict}'
```

```
Error Error
```

```bash
kubectl get deploy qwen3-canary-canary   # still there
```

**`Error`, not `Fail`.** They are different verdicts with different counters, and
the separation is the whole safety argument: a Prometheus outage says nothing
whatsoever about the canary. A single "strikes" counter cannot tell "the release
is bad" from "the monitoring is down" from "nobody is sending traffic" — and
those three call for a rollback, a retry, and a human respectively.

Bring it back:

```bash
kubectl -n monitoring scale sts prometheus-kps-kube-prometheus-stack-prometheus --replicas=1
```

The error counter resets on the first successful round and the rollout continues.

---

## 5. Autoscaling on the right signal (2 min)

```bash
kubectl apply -f config/samples/autoscaling_qwen3.yaml
kubectl wait modeldeployment/qwen3-auto --for=condition=Ready --timeout=300s

make load MD=qwen3-auto DURATION=10m CONCURRENCY=24 &

kubectl get modeldeployment qwen3-auto -w -o custom-columns=\
'DESIRED:.spec.replicas,READY:.status.readyReplicas,PERPOD:.status.autoscaling.currentMetricPerPod,MSG:.status.autoscaling.message'
```

**Queue depth, not CPU.** An inference server saturates its fixed number of
decode slots long before it saturates the CPU: with `--parallel 4`, the fifth
request waits while utilisation still reads something unremarkable. By the time
CPU crosses a 70% target, tail latency has been bad for minutes. Queue depth
crosses its threshold at the moment work actually starts *waiting*.

Stop the load and watch what does **not** happen:

```bash
kill %1
kubectl get modeldeployment qwen3-auto -w -o custom-columns=\
'DESIRED:.spec.replicas,MSG:.status.autoscaling.message'
```

It holds for five minutes before shrinking. The scale-up window is zero and the
scale-down window is 300s, deliberately asymmetric: adding capacity late costs
latency the user feels, removing it early costs a cold start on the next spike —
and a pod removed here has to load 400 MB of weights to come back.

### And a stock HPA drives the same resource

```bash
kubectl delete modeldeployment qwen3-auto
kubectl apply -f config/samples/qwen3_llamacpp.yaml
kubectl autoscale modeldeployment/qwen3 --min=1 --max=6 --cpu-percent=70
kubectl get hpa qwen3
```

```
NAME    REFERENCE                TARGETS       MINPODS   MAXPODS   REPLICAS
qwen3   ModelDeployment/qwen3    cpu: 12%/70%  1         6         3
```

`12%` and not `<unknown>` is the entire assertion. There is deliberately **no**
`mode: HPA` in the API: an external HPA writes `.spec.replicas` through the
/scale subresource and the controller reconciles it like any other spec change,
so the enum value would change zero lines of behaviour. A test proving a stock
HPA drives the CR is worth more than the enum — and the thing that makes it work
is that `status.selector` is a *serialized* selector string, which is the single
most common defect in a CRD /scale implementation.

---

## 6. SLOs (1 min)

Open **LLMCP — Inference SLO**.

```bash
kubectl get prometheusrule qwen3-slo -o yaml | head -40
```

The SLI is **time to first token**, not end-to-end latency. Duration scales with
how many tokens the caller asked for, so a p95 over mixed traffic measures the
request *mix* as much as the server: it moves when a client changes `max_tokens`
and stays flat through a real regression that coincides with shorter prompts.
TTFT and TPOT are length-independent, which is exactly why OpenTelemetry's
`gen_ai` conventions define them separately.

The burn-rate ladder is multiwindow — 14.4× over 1h *and* 5m to page, 6× over 6h
*and* 30m to ticket. Both windows must be burning: the long one establishes the
budget is genuinely being consumed, the short one establishes it still is.

Force an error burst:

```bash
kubectl patch modeldeployment qwen3 --type=merge -p '
{"spec":{"engine":{"env":[{"name":"LLMCP_FAKE_ERROR_RATE","value":"0.5"}]}}}'
```

The 14.4× alert appears in Prometheus's `/alerts` within about five minutes.

**One more panel worth naming:** `LLMCPNoTraffic`. The burn-rate alerts *cannot*
fire without traffic — with no requests both SLIs are ratios over a clamped
denominator, so they evaluate to a perfect 1.0 and stay silent. A deployment
whose Service selector broke would look flawless on every SLO panel. That alert
is the one that distinguishes "no problems" from "no data", which is the same
distinction `minRequestRate` draws for the canary and `Valid: false` draws for
the autoscaler.

---

## Cleanup

```bash
kubectl delete modeldeployment --all
make monitoring-uninstall
make kind-down
```

---

## If something goes wrong

| Symptom | Look at |
|---|---|
| `Ready` never goes True | `kubectl describe modeldeployment <name>` — the `ModelReady` condition names the source it is waiting on |
| Empty Grafana panels | `MetricsRegistered` condition; then Prometheus → Status → Targets |
| Canary never advances | `status.canary.checks[*].verdict`. `Inconclusive` almost always means too little traffic — is the load generator running? |
| Canary rolls back immediately | `status.canary.checks[*].message` names the metric and the value that failed |
| `kubectl get hpa` shows `<unknown>` | the pods need a CPU **request**; utilisation is a percentage of it |
| Load generator errors ~50% | you used `kubectl port-forward`. It pins a Service to one pod and drops connections under concurrency. Use `make load` |
