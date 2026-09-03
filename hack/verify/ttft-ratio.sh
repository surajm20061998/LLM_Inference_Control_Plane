#!/usr/bin/env bash
#
# Sprint 3 exit criterion: the anti-buffering assertion.
#
#   ttft_p95 < 0.5 x duration_p95, in Prometheus, under live streaming load.
#
# # Why this specific check
#
# Three separate mistakes in the shim produce the same silent failure: a missing
# httputil.ReverseProxy FlushInterval: -1, a transport that leaves gzip enabled,
# or timing the first response BYTE instead of the first content-bearing SSE
# frame. In every case the metric still exists, still has plausible values, and
# still moves under load — it has simply become a duplicate of the request
# duration.
#
# Nothing downstream would notice. Canary analysis would compare TTFT
# percentiles that are really duration percentiles, and since duration scales
# with output length, a canary serving longer answers would be rolled back for a
# latency regression it does not have. This check exists so that Sprint 4 is not
# built on a metric that means something other than its name.
#
# The 0.5 threshold is not arbitrary: with the project's default workload —
# roughly 64 output tokens — TTFT is a small fraction of the total, and any
# buffering pushes the ratio hard against 1.0. There is no realistic middle
# ground for it to land in.
#
# # Usage
#
#   make prometheus &          # port-forward, or set PROM_URL
#   make load MD=qwen &        # sustained streaming load
#   make verify-ttft
#
# Exits non-zero when the criterion is not met, so it can gate CI.
set -euo pipefail

PROM_URL="${PROM_URL:-http://localhost:9091}"
WINDOW="${WINDOW:-2m}"
RATIO_MAX="${RATIO_MAX:-0.5}"
MODEL="${MODEL:-.*}"
TIMEOUT="${TIMEOUT:-120}"
INTERVAL="${INTERVAL:-10}"

if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required" >&2
  exit 2
fi

# query runs an instant PromQL query and prints the first sample's value, or an
# empty string when the result vector is empty.
#
# An empty vector and a zero are deliberately distinguished everywhere below.
# They mean opposite things — "no data" versus "measured zero" — and collapsing
# them is precisely the class of bug this whole check exists to catch.
query() {
  local expr="$1"
  curl -sS -G --data-urlencode "query=${expr}" "${PROM_URL}/api/v1/query" \
    | jq -r '.data.result[0].value[1] // empty'
}

ttft_expr="histogram_quantile(0.95, sum by (le) (rate(llmcp_inference_ttft_seconds_bucket{model=~\"${MODEL}\"}[${WINDOW}])))"
dur_expr="histogram_quantile(0.95, sum by (le) (rate(llmcp_inference_request_duration_seconds_bucket{model=~\"${MODEL}\"}[${WINDOW}])))"
rate_expr="sum(rate(llmcp_inference_requests_total{model=~\"${MODEL}\"}[${WINDOW}]))"
stream_expr="sum(rate(llmcp_inference_ttft_seconds_count{model=~\"${MODEL}\"}[${WINDOW}]))"

echo "==> Prometheus: ${PROM_URL}"
echo "==> Waiting up to ${TIMEOUT}s for streaming load to appear..."

deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  req_rate="$(query "${rate_expr}")"
  stream_rate="$(query "${stream_expr}")"

  if [[ -n "${stream_rate}" ]] && awk -v v="${stream_rate}" 'BEGIN{exit !(v > 0)}'; then
    break
  fi

  if (( $(date +%s) >= deadline )); then
    echo
    echo "FAIL: no streaming requests were observed within ${TIMEOUT}s."
    echo "      request rate:   ${req_rate:-<no data>}"
    echo "      streamed rate:  ${stream_rate:-<no data>}"
    echo
    # This is the most common way to fail the check, and it is a load-generator
    # problem rather than a shim problem, so it is called out by name.
    echo "  A non-zero request rate with a zero STREAMED rate means the load"
    echo "  generator is not sending \"stream\": true. TTFT is only recorded for"
    echo "  streamed responses — deliberately, because for a non-streaming"
    echo "  request the first token and the last arrive together, and recording"
    echo "  that would poison the histogram with the total duration."
    exit 1
  fi
  sleep "${INTERVAL}"
done

ttft="$(query "${ttft_expr}")"
dur="$(query "${dur_expr}")"

if [[ -z "${ttft}" || -z "${dur}" ]]; then
  echo "FAIL: a percentile query returned no data (ttft=${ttft:-<none>} duration=${dur:-<none>})."
  exit 1
fi

# NaN is what histogram_quantile returns when every bucket is empty. It compares
# false against everything, so an unguarded numeric test would report a PASS.
if [[ "${ttft}" == "NaN" || "${dur}" == "NaN" ]]; then
  echo "FAIL: a percentile is NaN, which means the histogram has no observations in [${WINDOW}]."
  exit 1
fi

awk -v ttft="${ttft}" -v dur="${dur}" -v maxr="${RATIO_MAX}" -v rate="${req_rate:-0}" '
BEGIN {
  printf "\n"
  printf "  request rate     %8.2f req/s\n", rate
  printf "  ttft_p95         %8.3f s\n", ttft
  printf "  duration_p95     %8.3f s\n", dur

  if (dur <= 0) {
    printf "\nFAIL: duration_p95 is zero; there is nothing to compare against.\n"
    exit 1
  }

  ratio = ttft / dur
  printf "  ratio            %8.3f  (must be < %.2f)\n\n", ratio, maxr

  if (ratio < maxr) {
    printf "PASS: time to first token is a genuine fraction of total duration.\n"
    printf "      The SSE stream is reaching the client incrementally.\n"
    exit 0
  }

  printf "FAIL: ttft_p95 is %.0f%% of duration_p95.\n\n", ratio * 100
  printf "  A ratio at or near 1.0 means the first token arrived with the last,\n"
  printf "  i.e. the response was buffered somewhere in the path. Check, in order:\n"
  printf "    1. httputil.ReverseProxy FlushInterval is -1 (cmd/shim/proxy.go)\n"
  printf "    2. Transport.DisableCompression is true — gzip is a block compressor\n"
  printf "    3. TTFT is taken at the first content-bearing SSE frame, not the\n"
  printf "       first byte and not the role-only opening frame (cmd/shim/sse.go)\n"
  printf "    4. The load generator sends \"stream\": true\n"
  exit 1
}
'
