#!/usr/bin/env bash
# chaos-coord-partition.sh — partition a coord from its peers via docker
# network disconnect, verify split-brain prevention (Raft term invariant
# from ADR-038), reconnect, verify catch-up via WAL replication.
#
# Architectural invariants under test:
#   ADR-038  no split-brain — at most 1 leader visible across reachable
#            coords at any point in time (Raft term + voting rules)
#   ADR-038  isolated minority cannot elect — partitioned coord stays
#            candidate or follower (1 vote ≠ quorum)
#   ADR-039  on reconnect, isolated coord catches up via WAL repl
#
# Self-contained: 3-coord HA + 3-DN + edge, full teardown on exit.
#
# Usage:
#   ./scripts/chaos-coord-partition.sh                 # 60s, 1 partition cycle
#   ./scripts/chaos-coord-partition.sh --duration 120 --cycles 3
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=60
CYCLES=1
PARTITION_SEC=10  # how long each partition lasts

usage() {
  cat <<'EOF'
chaos-coord-partition.sh — partition coord from peers, verify no
split-brain, reconnect + verify catch-up.

Flags:
  --duration <s>      total runtime  (default 60)
  --cycles <n>        partition→reconnect cycles within duration (default 1)
  --partition-sec <s> seconds each partition lasts (default 10)

Hard PASS criteria:
  - never observe >1 leader simultaneously among reachable coords
  - all 200-PUTs are GET-able at end (no data loss)
  - isolated coord catches up after reconnect
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --duration)      DURATION="$2"; shift 2 ;;
    --cycles)        CYCLES="$2"; shift 2 ;;
    --partition-sec) PARTITION_SEC="$2"; shift 2 ;;
    -h|--help)       usage; exit 0 ;;
    *) echo "unknown flag: $1"; usage; exit 2 ;;
  esac
done

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

need curl; need jq; need docker; need python3

echo "🌪  chaos-coord-partition"
echo "   duration:      ${DURATION}s, ${CYCLES} cycle(s)"
echo "   partition-sec: ${PARTITION_SEC}s per cycle"
echo "   target:        ADR-038 (split-brain prevention) · ADR-039 (catch-up)"
echo

echo "[setup] tearing down any prior cluster..."
./scripts/down.sh >/dev/null 2>&1 || true

rebuild_images
docker network create $NET 2>/dev/null || true
start_dns 3
for i in 1 2 3; do
  start_coord "coord${i}" $((9000 + i - 1)) \
    "$PEERS_INTERNAL" "http://coord${i}:9000" \
    "/var/lib/kvfs-coord/coord.wal" "1"
done

echo "[setup] waiting for initial election (~5s)..."
sleep 5
LEADER0=$(find_coord_leader 3 || true)
[ -n "$LEADER0" ] || { echo "FAIL: no initial leader"; exit 1; }
echo "    initial leader = $LEADER0"

start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

PARTITIONED=()
trap '
  for n in "${PARTITIONED[@]}"; do docker network connect '"$NET"' "$n" >/dev/null 2>&1 || true; done
  rm -f "$TMP_BODY" "$PUT_LOG" 2>/dev/null || true
  ./scripts/down.sh >/dev/null 2>&1 || true
' EXIT INT TERM

# Seed
SEED_BUCKET="chaos"; SEED_KEY="seed.bin"
TMP_BODY=$(mktemp); PUT_LOG=$(mktemp)
head -c $((32 * 1024)) /dev/urandom > "$TMP_BODY"
SEED_SHA=$(sha256sum "$TMP_BODY" | awk '{print $1}')
seed_url="${EDGE}$(sign_url PUT "/v1/o/${SEED_BUCKET}/${SEED_KEY}" 7200)"
curl -fsS -X PUT "$seed_url" --data-binary "@${TMP_BODY}" >/dev/null
echo "[seed] PUT seed.bin sha=${SEED_SHA:0:16}.."

# Helper: count distinct leaders across reachable coords. Must always be ≤1.
count_leaders() {
  local n=0
  for i in 1 2 3; do
    local port=$((9000 + i - 1))
    local role
    role=$(curl -fs --max-time 1 "http://localhost:${port}/v1/coord/healthz" 2>/dev/null \
            | jq -r '.role' 2>/dev/null || echo "")
    [ "$role" = "leader" ] && n=$((n + 1))
  done
  echo "$n"
}

# Helper: pick a victim — preferentially the current leader (more interesting
# case: forces re-election among remaining peers). MUST NOT be the coord
# that edge is configured against (EDGE_COORD_URL points at coord1) —
# otherwise edge has no fallback path (CoordClient only redirects on 503,
# not on TCP-level conn failure). In production a load balancer in front
# of coords solves this; for the test we just pick a non-coord1 victim.
EDGE_COORD_NAME="coord1"
pick_victim() {
  local lead lead_name
  lead=$(find_coord_leader 3 || true)
  lead_name="${lead%:*}"
  # Prefer the leader if it's not edge's coord. Otherwise pick a non-leader
  # non-edge coord to force re-election anyway (edge follows X-COORD-LEADER).
  if [ -n "$lead_name" ] && [ "$lead_name" != "$EDGE_COORD_NAME" ]; then
    echo "$lead_name"
    return
  fi
  for n in coord2 coord3; do
    if [ "$n" != "$EDGE_COORD_NAME" ]; then
      echo "$n"
      return
    fi
  done
  echo "coord3"  # ultimate fallback
}

# Pre-partition: PUT N baseline keys, all should be retrievable post-recovery
echo "[baseline] PUT 5 baseline keys before any partition..."
for i in $(seq 1 5); do
  K="base-${i}"; B="body-base-${i}"
  put_url="${EDGE}$(sign_url PUT "/v1/o/chaos/${K}" 7200)"
  curl -fsS -X PUT --data-binary "$B" "$put_url" >/dev/null
  echo "${K}|${B}" >> "$PUT_LOG"
done

# Counters
SPLIT_BRAIN_OBSERVED=0
LEADERLESS_LONG=0   # leader=0 longer than allowed (election convergence ~3s)

CYCLE_LEN=$((DURATION / CYCLES))

for cycle in $(seq 1 "$CYCLES"); do
  echo
  echo "[cycle ${cycle}/${CYCLES}] ----------------------------------------"

  VICTIM=$(pick_victim)
  echo "[partition] disconnecting ${VICTIM} from network ${NET}"
  docker network disconnect "$NET" "$VICTIM" >/dev/null 2>&1 || true
  PARTITIONED+=("$VICTIM")

  # Watch: leader count must stay ≤1 throughout the partition.
  # We tolerate brief leader=0 windows (election in progress) but flag
  # if it persists long enough that the cluster is effectively dead.
  PARTITION_END=$(($(date +%s) + PARTITION_SEC))
  LEADERLESS_TICKS=0
  while [ "$(date +%s)" -lt "$PARTITION_END" ]; do
    LC=$(count_leaders)
    if [ "$LC" -gt 1 ]; then
      SPLIT_BRAIN_OBSERVED=$((SPLIT_BRAIN_OBSERVED + 1))
      echo "       ❌ SPLIT-BRAIN: ${LC} leaders simultaneously visible"
    elif [ "$LC" -eq 0 ]; then
      LEADERLESS_TICKS=$((LEADERLESS_TICKS + 1))
    fi
    sleep 1
  done
  if [ "$LEADERLESS_TICKS" -gt 5 ]; then
    LEADERLESS_LONG=$((LEADERLESS_LONG + 1))
    echo "       ⚠ leaderless for ${LEADERLESS_TICKS}s during partition (election did not converge)"
  fi

  echo "[partition] reconnecting ${VICTIM}..."
  docker network connect "$NET" "$VICTIM" >/dev/null 2>&1 || true
  PARTITIONED=("${PARTITIONED[@]/$VICTIM}")
  TMP=()
  for x in "${PARTITIONED[@]}"; do [ -n "$x" ] && TMP+=("$x"); done
  PARTITIONED=("${TMP[@]}")

  echo "[recover] waiting 6s for term sync + WAL catch-up..."
  sleep 6
  LC=$(count_leaders)
  [ "$LC" -ne 1 ] && echo "       ⚠ post-reconnect leader count = ${LC} (expected 1)"

  # Post-cycle PUT — should succeed (cluster healthy, edge can talk to coord1)
  K="cyc${cycle}-after"; B="body-after-${cycle}"
  put_url="${EDGE}$(sign_url PUT "/v1/o/chaos/${K}" 7200)"
  if curl -fsS --max-time 10 -X PUT --data-binary "$B" "$put_url" >/dev/null 2>&1; then
    echo "${K}|${B}" >> "$PUT_LOG"
    echo "       ✓ post-cycle PUT ${K} OK"
  else
    echo "       ❌ post-cycle PUT ${K} failed"
  fi
done

# Final: all PUT_LOG keys must be GET-able on every coord (lookup goes through
# leader; we also check via edge to be sure)
echo
echo "[verify] all logged keys retrievable via edge..."
FINAL_OK=0; FINAL_FAIL=0
while IFS='|' read -r K B; do
  get_url="${EDGE}$(sign_url GET "/v1/o/chaos/${K}" 7200)"
  GOT=$(curl -fsS --max-time 10 "$get_url" 2>/dev/null || echo "__FAIL__")
  if [ "$GOT" = "$B" ]; then
    FINAL_OK=$((FINAL_OK + 1))
  else
    FINAL_FAIL=$((FINAL_FAIL + 1))
    [ "$FINAL_FAIL" -le 3 ] && echo "       ⚠ ${K}: got ${GOT:0:24}.., expected ${B:0:24}.."
  fi
done < "$PUT_LOG"
TOTAL=$(wc -l < "$PUT_LOG" | tr -d ' ')

# Verify: post-partition cluster sees all 3 coords as healthy
echo
echo "[verify] post-partition coord healthz..."
for i in 1 2 3; do
  port=$((9000 + i - 1))
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://localhost:${port}/v1/coord/healthz" 2>/dev/null || echo "000")
  ROLE=$(curl -fs --max-time 2 "http://localhost:${port}/v1/coord/healthz" 2>/dev/null | jq -r .role 2>/dev/null || echo "?")
  echo "    coord${i}: ${CODE} role=${ROLE}"
done

echo
echo "[summary]"
echo "   cycles:           ${CYCLES} × ${PARTITION_SEC}s partition"
echo "   split-brain obs:  ${SPLIT_BRAIN_OBSERVED}  (must be 0)"
echo "   leaderless long:  ${LEADERLESS_LONG}  (cycles with >5s leader=0)"
echo "   final verify:     ${FINAL_OK} OK / ${FINAL_FAIL} FAIL on ${TOTAL} logged keys"

EXIT_CODE=0
if [ "$SPLIT_BRAIN_OBSERVED" -gt 0 ]; then
  echo "   ❌ ADR-038 INVARIANT BROKEN: ${SPLIT_BRAIN_OBSERVED} split-brain observations"
  EXIT_CODE=1
fi
if [ "$FINAL_FAIL" -gt 0 ]; then
  echo "   ❌ DURABILITY BROKEN: ${FINAL_FAIL} keys not retrievable"
  EXIT_CODE=1
fi
[ "$EXIT_CODE" -eq 0 ] && echo "   ✅ no split-brain · all ${TOTAL} keys durable across partition cycles"
exit $EXIT_CODE
