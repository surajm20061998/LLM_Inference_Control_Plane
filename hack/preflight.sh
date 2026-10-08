#!/usr/bin/env bash
#
# Refuse to start a local e2e environment the machine cannot afford.
#
# Why this exists: on a laptop, the failure mode of "not enough room" is not a
# clean error. Docker Desktop's virtual disk can be allowed to grow larger than
# the free space actually left on the host; when the host fills first, writes
# inside the VM fail with EIO, the image store is damaged, and the daemon
# crashes mid-build. That happened to this project, so the budget is checked
# BEFORE anything is created rather than discovered afterwards.
#
# Budget, measured on 2026-10-08 (kind 3 nodes, cold caches, after the
# Dockerfile cache-mount change):
#
#   step                                   Docker disk   container memory
#   kind-up + dev-images + dev-deploy        +5 GB          1.4 GiB
#   + monitoring-install + canary suites     +4 GB          3.0 GiB peak (07)
#
# A cold build of all four Go images leaves ~3 GB of BuildKit cache; before
# the cache mounts it was ~1.2 GB per image per source change, unbounded.
#
# On macOS the host also pays for whatever the VM uses beyond free RAM, in
# swap files on the same disk — several GB on a machine already near its RAM —
# which is why the free-disk floor sits well above the Docker growth alone.
#
# Thresholds are overridable, and the check is advisory when FORCE=1.
#
set -o errexit
set -o nounset
set -o pipefail

MIN_FREE_DISK_GB="${MIN_FREE_DISK_GB:-15}"
MIN_DOCKER_MEM_GB="${MIN_DOCKER_MEM_GB:-6}"
CLUSTER_NAME="${CLUSTER_NAME:-llmcp}"
FORCE="${FORCE:-0}"

failures=0
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; failures=$((failures + 1)); }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[1;32m ok \033[0m %s\n' "$*"; }

if ! docker info >/dev/null 2>&1; then
  fail "Docker is not running"
  exit 1
fi

# --- Docker memory ----------------------------------------------------------
docker_mem_gb=$(( $(docker info --format '{{.MemTotal}}') / 1073741824 ))
if (( docker_mem_gb < MIN_DOCKER_MEM_GB )); then
  fail "Docker has ${docker_mem_gb} GiB of memory; the canary suites need ${MIN_DOCKER_MEM_GB} GiB"
else
  ok "Docker memory ${docker_mem_gb} GiB (need ${MIN_DOCKER_MEM_GB})"
fi

# --- Free disk where Docker's data actually lives ---------------------------
case "$(uname -s)" in
  Darwin)
    docker_data="${HOME}/Library/Containers/com.docker.docker/Data"
    host_ram_gb=$(( $(sysctl -n hw.memsize) / 1073741824 ))
    # A VM allowed most of the host's RAM pushes everything else into swap,
    # and on macOS swap is files on the same disk this script is protecting.
    if (( docker_mem_gb * 2 > host_ram_gb )); then
      warn "Docker may use ${docker_mem_gb} of ${host_ram_gb} GiB host RAM; expect heavy swap (8 GiB is enough for this project)"
    fi
    if [[ "$(sysctl -n kern.memorystatus_vm_pressure_level 2>/dev/null || echo 1)" -ge 2 ]]; then
      warn "macOS already reports memory pressure; close other heavy apps first"
    fi
    raw="${docker_data}/vms/0/data/Docker.raw"
    if [[ -f "${raw}" ]]; then
      # ls reports the virtual disk LIMIT, du what is actually allocated.
      limit_gb=$(( $(stat -f %z "${raw}") / 1073741824 ))
      used_gb=$(( $(du -sk "${raw}" | awk '{print $1}') / 1048576 ))
    fi
    ;;
  *)
    docker_data="$(docker info --format '{{.DockerRootDir}}')"
    ;;
esac
[[ -d "${docker_data}" ]] || docker_data="/"
free_gb=$(df -Pk "${docker_data}" | awk 'NR == 2 { print int($4 / 1048576) }')
if (( free_gb < MIN_FREE_DISK_GB )); then
  fail "${free_gb} GB free on the disk holding Docker's data; need ${MIN_FREE_DISK_GB} GB"
else
  ok "${free_gb} GB free on the disk holding Docker's data (need ${MIN_FREE_DISK_GB})"
fi
if [[ -n "${limit_gb:-}" ]] && (( limit_gb > used_gb + free_gb )); then
  warn "Docker's virtual disk may grow to ${limit_gb} GB but only $(( used_gb + free_gb )) GB exist for it; Docker will fill the host disk before it hits its own limit. Lower it in Docker Desktop > Settings > Resources"
fi

# --- Competing clusters -----------------------------------------------------
others=$(docker ps --filter label=io.x-k8s.kind.cluster \
  --format '{{.Label "io.x-k8s.kind.cluster"}}' | sort -u | grep -vx "${CLUSTER_NAME}" || true)
if [[ -n "${others}" ]]; then
  warn "other kind clusters are running and share the same budget: $(tr '\n' ' ' <<< "${others}")"
fi

# --- Reclaimable build cache ------------------------------------------------
cache=$(docker system df --format '{{.Type}} {{.Reclaimable}}' | awk '$1 == "Build" { print $3 }')
[[ -n "${cache}" ]] && ok "reclaimable build cache: ${cache} (docker builder prune)"

if (( failures > 0 )); then
  if [[ "${FORCE}" == "1" ]]; then
    warn "continuing despite ${failures} failed check(s) because FORCE=1"
  else
    echo "Refusing to start: ${failures} failed check(s). Free resources, or override with FORCE=1." >&2
    exit 1
  fi
fi
