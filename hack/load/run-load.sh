#!/usr/bin/env bash
#
# Drive sustained streaming load at a ModelDeployment, from INSIDE the cluster.
#
# # Why a Job and not a port-forward
#
# `kubectl port-forward svc/...` is the obvious thing to reach for and it is
# actively wrong here. Port-forward resolves the Service to ONE pod and pins
# every connection to it for the lifetime of the forward. Under concurrency that
# produces two failures at once: connections are dropped when the single pod's
# keep-alive budget runs out — measured at a 50% error rate during this
# project's feasibility spike — and, worse, all load lands on one variant. A
# canary that received no traffic looks perfectly healthy right up to the moment
# it is promoted.
#
# Running the generator as an in-cluster Job means kube-proxy distributes
# connections exactly as it would for a real client, which is the behaviour the
# rollout logic has to be correct against.
#
# # Usage
#
#   make load MD=qwen
#   MD=qwen DURATION=5m CONCURRENCY=16 KEEPALIVE=false bash hack/load/run-load.sh
set -euo pipefail

MD="${MD:-}"
NAMESPACE="${NAMESPACE:-default}"
LOADGEN_IMG="${LOADGEN_IMG:-localhost:5001/llmcp-loadgen:dev}"
DURATION="${DURATION:-3m}"
CONCURRENCY="${CONCURRENCY:-8}"
TOKENS="${TOKENS:-64}"
# Keep-alive OFF by default.
#
# kube-proxy load-balances per CONNECTION, not per request. With keep-alive on,
# N concurrent clients open N long-lived connections, each pinned to one pod for
# the whole run — so at a requested canary weight of 20% the realised split is
# an arbitrary multiple of 1/N, stable and wrong for the entire analysis window.
# Disabling keep-alive is the documented mitigation for replica-based traffic
# splitting, and it costs a TCP handshake per request, which is noise next to a
# CPU inference call.
KEEPALIVE="${KEEPALIVE:-false}"
WAIT="${WAIT:-true}"

if [[ -z "${MD}" ]]; then
  echo "usage: MD=<modeldeployment-name> [NAMESPACE=default] $0" >&2
  exit 2
fi

MODEL="$(kubectl -n "${NAMESPACE}" get modeldeployment "${MD}" -o jsonpath='{.spec.model.name}')"
PORT="$(kubectl -n "${NAMESPACE}" get modeldeployment "${MD}" -o jsonpath='{.spec.serving.port}')"
PORT="${PORT:-8080}"

if [[ -z "${MODEL}" ]]; then
  echo "error: could not read .spec.model.name from modeldeployment/${MD}" >&2
  exit 1
fi

JOB="llmcp-load-${MD}-$(date +%s)"
URL="http://${MD}.${NAMESPACE}.svc:${PORT}"

echo "==> Job ${JOB}"
echo "    target      ${URL}"
echo "    model       ${MODEL}"
echo "    duration    ${DURATION}  concurrency ${CONCURRENCY}  tokens ${TOKENS}"
echo "    keep-alive  ${KEEPALIVE}"

kubectl -n "${NAMESPACE}" apply -f - <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB}
  labels:
    llmcp.io/loadgen: "true"
    llmcp.io/model-deployment: ${MD}
spec:
  # No retries. A load generator that restarts on failure produces a second
  # overlapping run whose samples are silently merged into the first, and the
  # resulting percentiles describe neither.
  backoffLimit: 0
  ttlSecondsAfterFinished: 600
  template:
    metadata:
      labels:
        llmcp.io/loadgen: "true"
    spec:
      restartPolicy: Never
      containers:
        - name: loadgen
          image: ${LOADGEN_IMG}
          imagePullPolicy: Always
          args:
            - --url=${URL}
            - --model=${MODEL}
            - --duration=${DURATION}
            - --concurrency=${CONCURRENCY}
            - --tokens=${TOKENS}
            - --keepalive=${KEEPALIVE}
            - --label=${MD}
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              memory: 256Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            capabilities:
              drop: ["ALL"]
YAML

if [[ "${WAIT}" != "true" ]]; then
  echo "==> Not waiting. Follow with: kubectl -n ${NAMESPACE} logs -f job/${JOB}"
  exit 0
fi

echo "==> Waiting for the run to finish (kubectl -n ${NAMESPACE} logs -f job/${JOB})"
kubectl -n "${NAMESPACE}" wait --for=condition=complete "job/${JOB}" --timeout=30m
kubectl -n "${NAMESPACE}" logs "job/${JOB}"
