# SLOs, error budgets and the alerts built on them

This is the runbook every generated alert links to.

## The short version

| | |
|---|---|
| **Latency SLI** | share of requests whose **first token** arrived within the threshold |
| **Availability SLI** | share of requests not answered with a **5xx** |
| **Alerts** | multiwindow burn rate: 14.4× (page) and 6× (ticket) |
| **Not an SLI** | end-to-end request duration — see below |

## Why TTFT and not latency

End-to-end request duration is the obvious latency SLI and it is the wrong one
for a language model, because **it scales with how many tokens the caller asked
for**.

A p95 over mixed traffic therefore measures the request *mix* as much as the
server. It moves when a client changes `max_tokens` and stays flat through a
genuine regression that happens to coincide with shorter prompts. An SLO built
on it burns budget for reasons the service cannot act on, and — worse — stays
green through incidents it should have caught.

Time to first token is length-independent: it is how long the user waits before
anything happens. Time per output token is the other half, also
length-independent. That is exactly why OpenTelemetry's `gen_ai` semantic
conventions define the two as separate metrics rather than shipping one latency
number, and it is why this operator's SLI is TTFT.

Duration is still measured and still graphed — it is the right number for
capacity planning. It is simply not an objective.

## Why the threshold gets snapped

A latency SLI is a ratio over a histogram bucket:

```promql
sum(rate(llmcp_inference_ttft_seconds_bucket{le="1.5"}[1h]))
  / sum(rate(llmcp_inference_ttft_seconds_count[1h]))
```

Prometheus matches `le` as an **exact string**. A rule asking for `le="0.5"`
against a histogram whose nearest boundaries are `0.4` and `0.6` does not
approximate and does not error — it returns an empty vector. The recording rule
then produces nothing, the burn-rate alert compares against no data, and the
alert never fires.

**An SLO that cannot alert is worse than no SLO, because it is believed.**

So `spec.observability.slo.ttftThreshold` is snapped **up** to the nearest real
boundary, and the snapping is reported in the `MetricsRegistered` condition
rather than done silently. Up rather than down because snapping down would hold
a service to a stricter target than its owner declared.

The boundaries are defined once, in `internal/metrics/buckets.go`, and shared
between the shim that produces the histogram and the rule generator that queries
it.

## The burn-rate ladder

An error budget is `1 - objective`. A burn rate of 1.0 exhausts it exactly over
the SLO period; 14.4× exhausts a 30-day budget in about two days.

| Alert | Rate | Long window | Short window | Severity | Fires after ~ |
|---|---|---|---|---|---|
| `*FastBurn` | 14.4× | 1h | 5m | critical | 2% of budget |
| `*SlowBurn` | 6× | 6h | 30m | warning | 5% of budget |
| `*SlowBurnDaily` † | 3× | 1d | 2h | warning | 10% of budget |
| `*SlowBurnWeekly` † | 1× | 3d | 6h | info | 10% of budget |

† Off by default. See *compressed windows* below.

**Every rung requires BOTH windows to be burning.** The long window establishes
that the budget is genuinely being consumed at that rate; the short window
establishes that it still is. Without the short window an alert stays lit for an
hour after the incident ends, and people learn to ignore it.

The numbers come from the Google SRE workbook's worked example and are chosen so
each rung fires after a defined fraction of the budget is spent — not so that
they look tidy.

## Compressed windows, stated plainly

The 1d and 3d rungs are **disabled by default**, and this is a deliberate
trade rather than an oversight.

A demo cluster is minutes old. A 3d window on it has no data, so every panel and
alert built on one shows "No Data" — which looks worse than not having them and
teaches whoever is watching to ignore the alert list. Being explicit that the
demo's SLO window is compressed is more credible than shipping rules that cannot
evaluate.

For a real deployment, set:

```yaml
spec:
  observability:
    prometheusRule:
      longWindowAlerts: true
```

## The alert that fires when nothing is wrong

`LLMCPNoTraffic` exists because **the burn-rate alerts cannot fire without
traffic**, and that is arithmetic rather than a gap in them.

With no requests, both SLIs are ratios whose denominators are clamped away from
zero, so both evaluate to a perfect `1.0` and every budget alert stays silent. A
ModelDeployment whose Service selector broke, or whose shim stopped being
scraped, therefore looks flawless on every SLO panel.

`LLMCPNoTraffic` fires when the shims are reporting (so the pods exist and are
scraped) but no request has arrived for ten minutes. It is the alert that can
tell "no problems" from "no data" — the same distinction the canary analysis
draws with `minRequestRate`, and the same one the four-valued verdicts draw
between `Pass` and `Inconclusive`.

## Responding

### `LLMCPTTFTBudget*`

First tokens are arriving too slowly, too often.

1. **Is there a rollout in flight?** `kubectl get modeldeployment <name>` —
   a `Canarying` phase means the canary may be the cause. The canary dashboard
   overlays both variants' TTFT; if only the canary is slow, the analysis gate
   should already be counting failures.
2. **Is the fleet undersized?** Check `llmcp_inference_queue_depth`. A queue
   that is consistently above the autoscaling target while replicas sit at
   `maxReplicas` means the ceiling is the constraint.
3. **Is it thread oversubscription?** llama.cpp reads the *host's* `/proc` and
   is cgroup-unaware. If `spec.engine.resources.limits.cpu` is unset the
   operator cannot derive `-t`, and several replicas on one node will contend
   badly enough that percentiles become noise.
4. **Is it a cold start?** Newly-started pods have a cold KV cache. A burst of
   restarts shows up as a TTFT spike that recovers on its own.

### `LLMCPAvailabilityBudget*`

Requests are failing with 5xx.

1. `llmcp_shim_upstream_errors_total` separates *the engine is unreachable*
   from *the engine returned an error*. They have different causes.
2. `client_canceled` is counted separately and is **not** a fault — it is a
   caller that hung up. A spike there usually means a load generator's own
   timeout, not a service problem.
3. Check the engine container's logs: `kubectl logs <pod> -c engine`.

### `LLMCPNoTraffic`

Nothing is reaching the pods, but they are up and being scraped.

1. `kubectl get endpoints <name>` — an empty list means the Service selector
   matches nothing, or no pod is Ready.
2. Confirm callers are using the published endpoint:
   `kubectl get modeldeployment <name> -o jsonpath='{.status.endpoint}'`.
3. If this deployment is genuinely idle overnight, silence the alert for it
   rather than lowering the threshold — the threshold is what makes it useful
   for every other deployment.

## Configuring

```yaml
spec:
  observability:
    slo:
      # Snapped up to the nearest histogram boundary. Reported when it changes.
      ttftThreshold: "1500m"
      # 99% of requests must beat it. The budget is 1%.
      ttftObjective: "990m"
      # 99.5% must not 5xx.
      availabilityObjective: "995m"
    prometheusRule:
      enabled: true
      longWindowAlerts: false
      # For a Prometheus whose ruleSelector is not empty.
      labels:
        release: kps
```

A stricter objective makes every alert **more sensitive**, not merely stricter:
the budget shrinks, and each rung fires at `factor × budget`.
