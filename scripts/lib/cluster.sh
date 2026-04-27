# cluster.sh — shared bring-up helpers for Season 5+ multi-daemon demos.
# Source me; relies on common.sh's `need`. Caller sets NET (network name)
# and ensures images are built.
#
# Provides:
#   start_dns N          — start dn1..dnN with kvfs-dn:dev
#   start_coord <name> <port> [peers] [self_url] [wal] [txn]
#                        — start one coord container, all HA flags optional
#   start_edge <name> <port> [coord_url]
#                        — start one edge, EDGE_COORD_URL optional
#   wait_healthz <url>   — poll up to 10s for 200 from <url>
#   find_coord_leader N  — return "name:port" of the leader among coord1..coordN
#                          (uses /v1/coord/healthz role field, no side effects)
#
# All functions assume `docker` + `curl` + `jq` on PATH and that `NET` is
# set. Volumes follow the pattern <name>-data, removed by ./scripts/down.sh.

start_dns() {
  local n="$1"
  for i in $(seq 1 "$n"); do
    docker volume create "dn${i}-data" >/dev/null
    docker run -d --name "dn${i}" --network "$NET" \
      -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
      -v "dn${i}-data:/var/lib/kvfs-dn" \
      kvfs-dn:dev >/dev/null
  done
}

# ensure_images <target>... — `docker build` each missing kvfs-* image.
# Default targets if no args: kvfs-coord kvfs-edge kvfs-dn kvfs-cli.
# "Missing" only — see rebuild_images for chaos/CI freshness semantics.
ensure_images() {
  local targets=("$@")
  if [ "${#targets[@]}" -eq 0 ]; then
    targets=(kvfs-coord kvfs-edge kvfs-dn kvfs-cli)
  fi
  for tgt in "${targets[@]}"; do
    docker images --format '{{.Repository}}:{{.Tag}}' | grep -q "^${tgt}:dev$" \
      || { echo "==> building ${tgt} (one-time)"; docker build --target "${tgt}" -t "${tgt}:dev" . >/dev/null; }
  done
}

# rebuild_images <target>... — always run `docker build` (cached layers fast
# on no-source-change). Use this in chaos / CI scripts where the test
# would silently pass against a stale binary if `ensure_images` skipped.
# A real bug discovered 2026-04-27: a chaos test mis-flagged a "phantom
# commit" because the running kvfs-edge image predated P7-07/P7-08
# (CoordClient retry) — the test was honest, the deployment was stale.
rebuild_images() {
  local targets=("$@")
  if [ "${#targets[@]}" -eq 0 ]; then
    targets=(kvfs-coord kvfs-edge kvfs-dn kvfs-cli)
  fi
  for tgt in "${targets[@]}"; do
    echo "==> rebuilding ${tgt} (chaos/CI freshness)"
    docker build --target "${tgt}" -t "${tgt}:dev" . >/dev/null
  done
}

# start_coord name port [peers] [self_url] [wal_path] [txn_raft_bool]
# Caller may pre-export COORD_DNS to override the default 3-DN list, or
# COORD_DN_IO=1 to enable Season 6 Ep.2's coord-side DN I/O (rebalance/
# gc/repair apply paths).
start_coord() {
  local name="$1" port="$2" peers="${3:-}" self_url="${4:-}" wal="${5:-}" txn="${6:-}"
  local dns="${COORD_DNS:-dn1:8080,dn2:8080,dn3:8080}"
  local dnio="${COORD_DN_IO:-}"
  docker volume create "${name}-data" >/dev/null
  local args=(
    -d --name "$name" --network "$NET"
    -p "${port}:9000"
    -e COORD_ADDR=":9000"
    -e COORD_DATA_DIR="/var/lib/kvfs-coord"
    -e COORD_DNS="$dns"
    -v "${name}-data:/var/lib/kvfs-coord"
  )
  [ -n "$peers" ]    && args+=(-e COORD_PEERS="$peers")
  [ -n "$self_url" ] && args+=(-e COORD_SELF_URL="$self_url")
  [ -n "$wal" ]      && args+=(-e COORD_WAL_PATH="$wal")
  [ -n "$txn" ]      && args+=(-e COORD_TRANSACTIONAL_RAFT="$txn")
  [ -n "$dnio" ]     && args+=(-e COORD_DN_IO="$dnio")
  docker run "${args[@]}" kvfs-coord:dev >/dev/null
}

# start_edge name port [coord_url]
# Caller may pre-export EDGE_DNS to override the default 3-DN list (used
# by demos that need a different DN topology, e.g. demo-vav).
start_edge() {
  local name="$1" port="$2" coord_url="${3:-}"
  local dns="${EDGE_DNS:-dn1:8080,dn2:8080,dn3:8080}"
  docker volume create "${name}-data" >/dev/null
  local args=(
    -d --name "$name" --network "$NET"
    -p "${port}:8000"
    -e EDGE_ADDR=":8000"
    -e EDGE_DNS="$dns"
    -e EDGE_DATA_DIR="/var/lib/kvfs-edge"
    -e EDGE_URLKEY_SECRET="$SECRET"
    -v "${name}-data:/var/lib/kvfs-edge"
  )
  [ -n "$coord_url" ] && args+=(-e EDGE_COORD_URL="$coord_url")
  docker run "${args[@]}" kvfs-edge:dev >/dev/null
}

wait_healthz() {
  local url="$1"
  for _ in $(seq 1 30); do
    curl -fs "$url" >/dev/null 2>&1 && return 0
    sleep 0.3
  done
  echo "timeout waiting for $url" >&2
  return 1
}

# find_coord_leader N — N is the count (assumes coord1..coordN bound at
# 9000..9000+N-1). Returns "name:port" or non-zero on no leader.
find_coord_leader() {
  local n="$1"
  for i in $(seq 1 "$n"); do
    local port=$((9000 + i - 1))
    local role
    role=$(curl -fs "http://localhost:${port}/v1/coord/healthz" 2>/dev/null | jq -r .role 2>/dev/null || echo "")
    if [ "$role" = "leader" ]; then
      echo "coord${i}:${port}"
      return 0
    fi
  done
  return 1
}
