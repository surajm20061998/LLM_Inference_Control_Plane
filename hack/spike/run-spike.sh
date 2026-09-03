#!/usr/bin/env bash
#
# Closed-loop feasibility spike.
#
# This answers one question before Sprint 4 commits to building canary analysis:
# on a laptop, with a 0.6B model on CPU, is the TTFT signal actually good enough
# to promote or roll back a release on? A metric whose noise floor is wider than
# the regression it is meant to catch produces rollbacks at random, and that is
# far more expensive to discover after the controller is written.
#
# Six measurements, each with a kill threshold. See
# docs/adr/0001-spike-closed-loop.md for the results and what they decided.
#
# Usage:
#   hack/spike/run-spike.sh [--duration 90s] [--namespace default]
#
# Prerequisites: a kind cluster with the operator deployed and the real model
# image pushed (make kind-up model-image-real dev-deploy).
set -euo pipefail

DURATION="${DURATION:-90s}"
NAMESPACE="${NAMESPACE:-default}"
CONCURRENCY="${CONCURRENCY:-8}"
TOKENS="${TOKENS:-64}"
WINDOW="${WINDOW:-10s}"
# The noise run is longer than the others on purpose: the coefficient of
# variation is computed over per-window p95 values with the first and last
# window discarded, so a short run yields too few points to mean anything.
NOISE_DURATION="${NOISE_DURATION:-150s}"
OUT_DIR="${OUT_DIR:-.build/spike}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --duration) DURATION="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --concurrency) CONCURRENCY="$2"; shift 2 ;;
    --out) OUT_DIR="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

MODEL_IMAGE="${MODEL_IMAGE:-localhost:5001/llmcp-model:qwen3-0.6b-q4km}"
PRIMARY=spike-primary
CANARY=spike-canary

mkdir -p "$OUT_DIR"
echo "==> Output: $OUT_DIR"

cleanup() {
  kubectl delete job -n "$NAMESPACE" -l llmcp.io/spike=true --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# The two variants under test.
#
# The canary differs from the primary ONLY in its CPU limit: 500m against 2.
# The plan called for an injected-latency sidecar, but that sidecar does not
# exist until Sprint 3, and inducing the regression through a real resource
# limit is arguably the better test anyway — a synthetic sleep adds a constant,
# whereas CPU starvation degrades exactly the prompt-processing work that TTFT
# measures, which is what a genuine bad rollout looks like.
#
# Because the operator derives -t from the CPU limit, the canary also drops to
# one thread. That is the intended coupling, not a confound.
# ---------------------------------------------------------------------------
render_md() {
  local name="$1" replicas="$2" cpu_limit="$3" cpu_request="$4"
  cat <<YAML
apiVersion: inference.llmcp.io/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    llmcp.io/spike: "true"
spec:
  replicas: ${replicas}
  model:
    name: qwen3-0.6b
    source:
      image:
        image: ${MODEL_IMAGE}
        path: /weights/model.gguf
        pullPolicy: IfNotPresent
  engine:
    type: llamacpp
    contextSize: 4096
    maxConcurrency: 4
    resources:
      requests:
        cpu: "${cpu_request}"
        memory: 1Gi
      limits:
        cpu: "${cpu_limit}"
        memory: 2Gi
  serving:
    port: 8080
    startupTimeout: 240s
YAML
}

echo "==> Applying spike variants"
render_md "$PRIMARY" 2 "2" "1"     | kubectl apply -f -
render_md "$CANARY"  1 "500m" "250m" | kubectl apply -f -

# ---------------------------------------------------------------------------
# Measurement 6 — cold start to a passing /health.
#
# Taken first, because it is the only measurement that requires the pods to not
# already be running. Timing starts before the ModelDeployment reports Ready and
# ends when it does; the operator's Ready condition is gated on the engine's own
# /health, which returns 503 until the model is resident.
# ---------------------------------------------------------------------------
echo "==> [6] Cold start"

# Settle first, then force a genuine restart.
#
# Waiting on Ready straight after `kubectl apply` measures nothing when the pods
# are already running from an earlier run: the condition is already true, the
# wait returns instantly, and the result is a confident 0s. The first run of
# this spike reported exactly that. Deleting the pods and timing their
# replacements is the only way to make the measurement mean what it says.
kubectl wait modeldeployment/"$PRIMARY" -n "$NAMESPACE" --for=condition=Ready --timeout=600s
kubectl wait modeldeployment/"$CANARY" -n "$NAMESPACE" --for=condition=Ready --timeout=600s

kubectl delete pods -n "$NAMESPACE" -l llmcp.io/model-deployment="$PRIMARY" --wait=true >/dev/null
COLD_START_BEGIN=$(date +%s)
# rollout status returns when every replacement pod passes its readiness probe,
# which for this operator means llama.cpp's own /health returned 200 — the model
# is loaded and slots are available, not merely that the process started.
kubectl rollout status -n "$NAMESPACE" "deploy/${PRIMARY}-primary" --timeout=600s >/dev/null
COLD_START_S=$(( $(date +%s) - COLD_START_BEGIN ))
echo "    cold start: ${COLD_START_S}s"

# ---------------------------------------------------------------------------
# Measurement 1 — per-pod memory.
#
# memory.current is the container's cgroup charge. It INCLUDES the page cache
# backing the mmap'd GGUF, which is the honest number to compare against a
# memory limit, since that is what the kernel accounts against the limit. The
# anon figure from memory.stat is reported alongside it because it is what
# people mean by "RSS" and the two differ by roughly the model file size.
# ---------------------------------------------------------------------------
echo "==> [1] Per-pod memory"
mem_json="[]"
for md in "$PRIMARY" "$CANARY"; do
  for pod in $(kubectl get pods -n "$NAMESPACE" -l llmcp.io/model-deployment="$md" -o jsonpath='{.items[*].metadata.name}'); do
    current=$(kubectl exec -n "$NAMESPACE" "$pod" -c engine -- cat /sys/fs/cgroup/memory.current 2>/dev/null || echo 0)
    peak=$(kubectl exec -n "$NAMESPACE" "$pod" -c engine -- cat /sys/fs/cgroup/memory.peak 2>/dev/null || echo 0)
    anon=$(kubectl exec -n "$NAMESPACE" "$pod" -c engine -- sh -c "awk '/^anon /{print \$2}' /sys/fs/cgroup/memory.stat" 2>/dev/null || echo 0)
    mem_json=$(printf '%s' "$mem_json" | jq --arg md "$md" --arg pod "$pod" \
      --argjson cur "${current:-0}" --argjson peak "${peak:-0}" --argjson anon "${anon:-0}" \
      '. + [{model_deployment:$md, pod:$pod, memory_current_mib:(($cur/1048576)*100|round/100), memory_peak_mib:(($peak/1048576)*100|round/100), anon_mib:(($anon/1048576)*100|round/100)}]')
    echo "    $pod: current=$(( current / 1048576 ))MiB anon=$(( anon / 1048576 ))MiB"
  done
done
printf '%s\n' "$mem_json" > "$OUT_DIR/01-memory.json"

# The Docker VM total, which is the ceiling all of the above shares.
docker info --format '{{json .}}' 2>/dev/null \
  | jq '{mem_total_gib: ((.MemTotal/1073741824)*100|round/100), ncpu: .NCPU}' \
  > "$OUT_DIR/01-vm.json" || true

# ---------------------------------------------------------------------------
# Load is driven from INSIDE the cluster, as a Job.
#
# The obvious alternative — `kubectl port-forward` to each Service — is wrong
# twice over, and the first run of this spike found out the hard way.
#
# Correctness: port-forward resolves a Service to ONE pod and pins every request
# to it. kube-proxy is never in the path, so a traffic-split measurement taken
# that way measures kubectl, not Kubernetes.
#
# Reliability: port-forward multiplexes every connection over a single stream
# and drops them under concurrency. The first attempt at this spike recorded a
# 50% error rate, all of them bare `EOF` on the client side, with the engine
# pods entirely healthy. Latency percentiles computed from the surviving half of
# a run are not a measurement of anything.
# ---------------------------------------------------------------------------
LOADGEN_IMAGE="${LOADGEN_IMAGE:-localhost:5001/llmcp-loadgen:dev}"

# run_load <job-suffix> <service> <concurrency> <duration> <tokens> <keepalive> <outfile>
#
# Runs one load Job to completion and captures its JSON report. The report goes
# to stdout, so `kubectl logs` is the transport.
run_load() {
  local suffix="$1" svc="$2" conc="$3" dur="$4" tokens="$5" keepalive="$6" outfile="$7"
  local job="loadgen-${suffix}"

  kubectl delete job -n "$NAMESPACE" "$job" --ignore-not-found >/dev/null 2>&1

  kubectl apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job}
  namespace: ${NAMESPACE}
  labels:
    llmcp.io/spike: "true"
spec:
  # No retries. A retried run would silently concatenate two partial load
  # profiles into one report.
  backoffLimit: 0
  template:
    metadata:
      labels:
        llmcp.io/spike: "true"
    spec:
      restartPolicy: Never
      containers:
        - name: loadgen
          image: ${LOADGEN_IMAGE}
          imagePullPolicy: Always
          args:
            - -url=http://${svc}.${NAMESPACE}.svc:8080
            - -label=${suffix}
            - -concurrency=${conc}
            - -duration=${dur}
            - -tokens=${tokens}
            - -window=${WINDOW}
            - -keepalive=${keepalive}
          resources:
            requests:
              cpu: 200m
              memory: 64Mi
            limits:
              # Capped so the generator cannot starve the engines it is
              # measuring. On a laptop the load generator and the system under
              # test share one CPU pool, and an uncapped client turns a latency
              # measurement into a measurement of contention.
              cpu: "1"
              memory: 256Mi
YAML

  # Generous: the deadline has to cover image pull plus the run itself.
  kubectl wait -n "$NAMESPACE" --for=condition=Complete "job/${job}" --timeout=600s >/dev/null

  kubectl logs -n "$NAMESPACE" "job/${job}" > "$outfile"
  jq -e . "$outfile" >/dev/null || { echo "load job ${job} produced no valid JSON" >&2; cat "$outfile" >&2; exit 1; }
  kubectl delete job -n "$NAMESPACE" "$job" --ignore-not-found >/dev/null 2>&1
}

# Per-pod concurrency is held EQUAL across the two variants, so the only thing
# that differs between them is the CPU limit under test. The primary has two
# pods and the canary one, so the primary is driven at twice the concurrency.
# Sending identical total concurrency to both would have loaded the canary's
# single pod twice as hard as each primary pod, and the resulting "separation"
# would have been partly an artifact of that rather than of the regression.
PER_POD_CONCURRENCY="${PER_POD_CONCURRENCY:-4}"
PRIMARY_CONCURRENCY=$(( PER_POD_CONCURRENCY * 2 ))
CANARY_CONCURRENCY=$(( PER_POD_CONCURRENCY ))

# ---------------------------------------------------------------------------
# Measurement 3 — separation, on a strictly like-for-like comparison.
#
# This phase temporarily scales the primary down to ONE replica so that both
# variants are a single pod driven at the same concurrency, differing only by
# the CPU limit under test.
#
# The first version of this spike skipped that step and compared a two-pod
# primary at concurrency 8 against a one-pod canary at concurrency 4. The result
# was not merely noisy, it was INVERTED: the primary's p95 TTFT came out higher
# than the starved canary's. The cause is that kube-proxy distributes
# connections unevenly, so one primary pod received more than the four
# concurrent requests llama.cpp has slots for, and the excess queued. p95 TTFT
# then measured queueing on the primary and pure compute on the canary — two
# different quantities wearing the same name.
#
# Scaling through the ModelDeployment rather than the child Deployment is
# deliberate: it exercises the /scale subresource, which is the same path an
# HPA will take in a later sprint.
# ---------------------------------------------------------------------------
echo "==> [3] Separation: scaling primary to 1 replica for a like-for-like comparison"
kubectl scale modeldeployment/"$PRIMARY" -n "$NAMESPACE" --replicas=1 >/dev/null
kubectl rollout status -n "$NAMESPACE" "deploy/${PRIMARY}-primary" --timeout=300s >/dev/null

echo "    primary  (cpu=2,    1 pod, c=${PER_POD_CONCURRENCY})"
run_load sep-primary "$PRIMARY" "$PER_POD_CONCURRENCY" "$DURATION" "$TOKENS" true \
  "$OUT_DIR/03-sep-primary.json"

echo "    canary   (cpu=500m, 1 pod, c=${PER_POD_CONCURRENCY})"
run_load sep-canary "$CANARY" "$PER_POD_CONCURRENCY" "$DURATION" "$TOKENS" true \
  "$OUT_DIR/03-sep-canary.json"

echo "==> Restoring primary to 2 replicas"
kubectl scale modeldeployment/"$PRIMARY" -n "$NAMESPACE" --replicas=2 >/dev/null
kubectl rollout status -n "$NAMESPACE" "deploy/${PRIMARY}-primary" --timeout=300s >/dev/null

# ---------------------------------------------------------------------------
# Measurements 2 and 4 — throughput and noise, at the real two-pod shape.
#
# The noise run is the longest of the spike: the coefficient of variation is
# computed over per-window p95 values with the first and last window discarded,
# so a short run yields too few points to be a statistic.
# ---------------------------------------------------------------------------
echo "==> [2,4] Throughput and noise against primary (${NOISE_DURATION}, c=${PRIMARY_CONCURRENCY})"
run_load primary "$PRIMARY" "$PRIMARY_CONCURRENCY" "$NOISE_DURATION" "$TOKENS" true \
  "$OUT_DIR/02-primary.json"

# ---------------------------------------------------------------------------
# Measurement 5 — traffic split under keep-alive.
#
# kube-proxy balances per CONNECTION. With keep-alive on, N concurrent clients
# hold N connections, each pinned to whichever pod it first reached, for the
# whole run. If that produces a badly skewed split then a replica-based canary
# weight is not delivering the traffic it claims to, and analysis would compare
# a canary that received almost nothing against a primary that received almost
# everything — passing a canary that was never really tested.
#
# Requests per pod are derived from llama.cpp's own prompt-token counters. Every
# request in the run sends an identical prompt, so the token delta is exactly
# proportional to the request count.
# ---------------------------------------------------------------------------
split_snapshot() {
  local md="$1"
  for pod in $(kubectl get pods -n "$NAMESPACE" -l llmcp.io/model-deployment="$md" -o jsonpath='{.items[*].metadata.name}'); do
    local v
    v=$(kubectl exec -n "$NAMESPACE" "$pod" -c engine -- \
          sh -c 'curl -s localhost:8000/metrics' 2>/dev/null \
        | awk '/^llamacpp:prompt_tokens_total /{a=$2} /^llamacpp:prompt_tokens_cached_total /{b=$2} END{print (a+0)+(b+0)}')
    echo "$pod ${v:-0}"
  done
}

measure_split() {
  local keepalive="$1" outfile="$2"
  local before after
  before=$(split_snapshot "$PRIMARY")
  run_load "split-ka-${keepalive}" "$PRIMARY" "$PRIMARY_CONCURRENCY" "$DURATION" 16 "$keepalive" \
    "$OUT_DIR/05-load-keepalive-${keepalive}.json"
  after=$(split_snapshot "$PRIMARY")

  jq -n --arg ka "$keepalive" --arg before "$before" --arg after "$after" '
    def parse: [splits("\n")] | map(select(length>0) | split(" ") | {pod: .[0], v: (.[1]|tonumber)});
    ($before|parse) as $b | ($after|parse) as $a
    | [ $a[] | . as $x | {pod: $x.pod, delta: ($x.v - (($b[] | select(.pod==$x.pod) | .v) // 0))} ]
    | (map(.delta) | add) as $total
    | {keep_alive: ($ka=="true"), total_prompt_tokens: $total,
       pods: [ .[] | {pod: .pod, delta: .delta,
                      share_pct: (if $total > 0 then ((.delta/$total*1000)|round/10) else 0 end)} ]}
  ' > "$outfile"
}

echo "==> [5] Traffic split across 2 pods, keep-alive ON"
measure_split true "$OUT_DIR/05-split-keepalive-on.json"
echo "==> [5] Traffic split across 2 pods, keep-alive OFF"
measure_split false "$OUT_DIR/05-split-keepalive-off.json"

# Four pods, still eight connections. This is the harder case and the one that
# actually matters for a canary: two pods and eight connections can come out
# even by luck, whereas four pods is where per-connection pinning would show up
# as a visibly lumpy split. It is also the shape a 25% canary weight produces.
echo "==> [5] Traffic split across 4 pods, keep-alive ON"
kubectl scale modeldeployment/"$PRIMARY" -n "$NAMESPACE" --replicas=4 >/dev/null
kubectl rollout status -n "$NAMESPACE" "deploy/${PRIMARY}-primary" --timeout=300s >/dev/null
measure_split true "$OUT_DIR/05-split-4pods-keepalive-on.json"
kubectl scale modeldeployment/"$PRIMARY" -n "$NAMESPACE" --replicas=2 >/dev/null

# ---------------------------------------------------------------------------
# Roll it all up.
# ---------------------------------------------------------------------------
jq -n \
  --argjson cold "$COLD_START_S" \
  --slurpfile mem "$OUT_DIR/01-memory.json" \
  --slurpfile primary "$OUT_DIR/02-primary.json" \
  --slurpfile sepPrimary "$OUT_DIR/03-sep-primary.json" \
  --slurpfile sepCanary "$OUT_DIR/03-sep-canary.json" \
  --slurpfile splitOn "$OUT_DIR/05-split-keepalive-on.json" \
  --slurpfile splitOff "$OUT_DIR/05-split-keepalive-off.json" \
  --slurpfile split4 "$OUT_DIR/05-split-4pods-keepalive-on.json" '
  def errpct($o): (if $o.requests > 0 then (($o.errors / $o.requests * 1000)|round/10) else 0 end);
  def r2: (.*100|round/100);
  {
    m1_memory: {
      pods: $mem[0],
      max_current_mib: ([$mem[0][].memory_current_mib] | max),
      kill_threshold_mib: 2048
    },
    m2_throughput: {
      primary_rps: ($primary[0].overall.rps | r2),
      canary_rps: ($sepCanary[0].overall.rps | r2),
      kill_threshold_canary_rps: 0.5
    },
    m3_separation: {
      note: "one pod per side, equal concurrency; only the CPU limit differs",
      primary_ttft_p95_s: ($sepPrimary[0].overall.ttft_p95_s | r2),
      canary_ttft_p95_s: ($sepCanary[0].overall.ttft_p95_s | r2),
      ttft_ratio: (if $sepPrimary[0].overall.ttft_p95_s > 0
                   then (($sepCanary[0].overall.ttft_p95_s / $sepPrimary[0].overall.ttft_p95_s) | r2)
                   else 0 end),
      primary_duration_p95_s: ($sepPrimary[0].overall.duration_p95_s | r2),
      canary_duration_p95_s: ($sepCanary[0].overall.duration_p95_s | r2),
      duration_ratio: (if $sepPrimary[0].overall.duration_p95_s > 0
                       then (($sepCanary[0].overall.duration_p95_s / $sepPrimary[0].overall.duration_p95_s) | r2)
                       else 0 end),
      kill_threshold_ratio: 2.0
    },
    m4_noise: {
      primary_ttft_p95_cv: (($primary[0].ttft_p95_cv*1000)|round/1000),
      windows: ($primary[0].windows | length),
      kill_threshold_cv: 0.5
    },
    m5_traffic_split: {
      # skew is max_share / min_share. An even split is 1.0; per-connection
      # pinning would push it up without bound.
      two_pods_keep_alive_on: ($splitOn[0] + {skew: (([$splitOn[0].pods[].share_pct]|max) / ([$splitOn[0].pods[].share_pct]|min) | r2)}),
      two_pods_keep_alive_off: ($splitOff[0] + {skew: (([$splitOff[0].pods[].share_pct]|max) / ([$splitOff[0].pods[].share_pct]|min) | r2)}),
      four_pods_keep_alive_on: ($split4[0] + {skew: (([$split4[0].pods[].share_pct]|max) / ([$split4[0].pods[].share_pct]|min) | r2)})
    },
    m6_cold_start: { seconds: $cold, kill_threshold_s: 90 },
    error_rate_pct: {
      sep_primary: errpct($sepPrimary[0].overall),
      sep_canary: errpct($sepCanary[0].overall),
      noise_primary: errpct($primary[0].overall)
    },
    anti_buffering: {
      sep_primary_ttft_p95_over_duration_p95: ($sepPrimary[0].overall.ttft_p95_over_duration_p95 | r2)
    }
  }' > "$OUT_DIR/summary.json"

echo
echo "==> Summary"
cat "$OUT_DIR/summary.json"
