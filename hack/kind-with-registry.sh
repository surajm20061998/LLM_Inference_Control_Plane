#!/usr/bin/env bash
#
# Create the local kind cluster with a container registry attached.
#
# Why a registry rather than `kind load docker-image`:
#
#   `kind load` streams the ENTIRE image tarball into every node's containerd on
#   every invocation, with no layer deduplication. For the operator image (a
#   small static binary) that is merely slow. For a model image — hundreds of
#   megabytes of weights that never change while you iterate on Go code — it is
#   the difference between a two second and a forty second edit/deploy cycle.
#   A registry push transfers only changed layers.
#
#   It also makes imagePullPolicy behave the way it does in a real cluster,
#   which `kind load` does not.
#
# Adapted from the upstream kind local-registry recipe:
#   https://kind.sigs.k8s.io/docs/user/local-registry/
#
set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-llmcp}"
REGISTRY_NAME="${REGISTRY_NAME:-kind-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"

# Pinned by digest so that upgrading kind does not silently change the
# Kubernetes version under the project.
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.35.8}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

# ---------------------------------------------------------------------------
# 1. Registry
# ---------------------------------------------------------------------------
if [ "$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)" != 'true' ]; then
  log "Starting local registry ${REGISTRY_NAME} on localhost:${REGISTRY_PORT}"
  docker run \
    -d --restart=always \
    -p "127.0.0.1:${REGISTRY_PORT}:5000" \
    --network bridge \
    --name "${REGISTRY_NAME}" \
    registry:3
else
  log "Registry ${REGISTRY_NAME} already running"
fi

# ---------------------------------------------------------------------------
# 2. Cluster
# ---------------------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "Cluster ${CLUSTER_NAME} already exists"
else
  log "Creating kind cluster ${CLUSTER_NAME} (${NODE_IMAGE})"
  kind create cluster \
    --name "${CLUSTER_NAME}" \
    --image "${NODE_IMAGE}" \
    --config "${REPO_ROOT}/hack/kind-config.yaml" \
    --wait 120s
fi

# ---------------------------------------------------------------------------
# 3. Teach every node where localhost:5001 actually lives
# ---------------------------------------------------------------------------
# "localhost" inside a node's network namespace is the node itself, not the
# host, so the registry has to be referred to by its container name on the kind
# network. containerd resolves that through a per-registry hosts.toml.
REGISTRY_DIR="/etc/containerd/certs.d/localhost:${REGISTRY_PORT}"
for node in $(kind get nodes --name "${CLUSTER_NAME}"); do
  log "Configuring registry mirror on ${node}"
  docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
  docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml" <<EOF
[host."http://${REGISTRY_NAME}:5000"]
EOF
done

# ---------------------------------------------------------------------------
# 4. Join the registry to the kind network
# ---------------------------------------------------------------------------
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${REGISTRY_NAME}")" = 'null' ]; then
  log "Connecting ${REGISTRY_NAME} to the kind network"
  docker network connect kind "${REGISTRY_NAME}"
fi

# ---------------------------------------------------------------------------
# 5. Advertise the registry (KEP-1755) so tooling can discover it
# ---------------------------------------------------------------------------
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

log "Cluster ${CLUSTER_NAME} is ready"
kubectl --context "kind-${CLUSTER_NAME}" get nodes -o wide
