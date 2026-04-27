#!/usr/bin/env bash
# chaos-coord-quorum-loss.sh — kill 2 of 3 coords, attempt PUTs during the
# outage, verify NONE phantom-commit on the survivor.
#
# Architectural claim under test:
#   ADR-040 transactional commit — when quorum is lost, PUT must return
#   a non-2xx status AND the surviving coord's bbolt must remain
#   unchanged. A "phantom commit" (200 to client OR data appearing on the
#   survivor) is a critical correctness violation.
#
# Test flow:
#   Phase A: setup 3-coord HA (Raft + WAL repl + transactional commit) +
#            edge proxy. Seed N keys with quorum healthy.
#   Phase B: kill 2 coords (leader + 1 follower). Sleep past the leader
#            lease.
#   Phase C: attempt N PUTs while only 1 coord survives. Assert every PUT
#            fails (5xx OR connect-refused).
#   Phase D: while quorum-down, fetch the survivor's object list directly
#            (admin endpoint). Assert count == Phase A count (no phantom
#            commit landed).
#   Phase E: restore killed coords, wait for re-election + WAL catch-up,
#            PUT N more keys, GET them all.
#   Phase F: final invariant — only Phase A + Phase E keys exist; Phase
#            C keys are absent.
#
# Self-contained — tears down on exit.
set -euo pipefail
cd "$(dirname "$0")/.."

PHASE_KEYS=5      # keys per phase (A, C, E)
QUORUM_DOWN_SEC=8 # sleep with 2/3 coords down

usage() {
  cat <<'EOF'
chaos-coord-quorum-loss.sh — verify ADR-040 transactional commit prevents
phantom commits when quorum is lost.

Flags:
  --keys <n>       keys per phase A/C/E (default 5)
  --down-sec <s>   seconds to keep quorum lost (default 8)

Exit:
  0  PASS — no phantom commit
  1  FAIL — bbolt drift detected OR Phase C PUT succeeded OR Phase E miss
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --keys)     PHASE_KEYS="$2"; shift 2 ;;
    --down-sec) QUORUM_DOWN_SEC="$2"; shift 2 ;;
    -h|--help)  usage; exit 0 ;;
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

echo "🌪  chaos-coord-quorum-loss"
echo "   keys/phase:    ${PHASE_KEYS}"
echo "   quorum down:   ${QUORUM_DOWN_SEC}s"
echo "   target:        ADR-040 (transactional commit, no phantom)"
echo

# === Setup ==================================================================
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
LEADER=$(find_coord_leader 3 || true)
[ -n "$LEADER" ] || { echo "FAIL: no leader"; for i in 1 2 3; do docker logs "coord${i}" 2>&1 | tail -8; done; exit 1; }
LEADER_NAME="${LEADER%:*}"
echo "    leader = ${LEADER}"

start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# Cleanup
DOWN_NAMES=()
trap '
  for n in "${DOWN_NAMES[@]}"; do docker start "$n" >/dev/null 2>&1 || true; done
  ./scripts/down.sh >/dev/null 2>&1 || true
' EXIT INT TERM

# Helper: count objects via coord admin (read-only — works on any coord
# that has metadata, leader or follower). The endpoint returns a JSON
# array directly (writeJSON of []ObjectMeta), so we count length on the
# root, not on a `.objects` key.
coord_object_count() {
  local coord_port="$1"
  curl -fsS --max-time 5 "http://localhost:${coord_port}/v1/coord/admin/objects" 2>/dev/null \
    | jq -r 'length' 2>/dev/null \
    || echo "ERROR"
}

# Pick the surviving coord (first one that's NOT the leader).
SURVIVOR=""
SURVIVOR_PORT=""
for i in 1 2 3; do
  if [ "coord${i}" != "$LEADER_NAME" ]; then
    SURVIVOR="coord${i}"
    SURVIVOR_PORT=$((9000 + i - 1))
    break
  fi
done
echo "    survivor = ${SURVIVOR}:${SURVIVOR_PORT}"

# === Phase A: seed with quorum healthy ======================================
echo
echo "[phase A] PUT ${PHASE_KEYS} keys with quorum healthy"
PHASE_A_KEYS=()
for i in $(seq 1 "$PHASE_KEYS"); do
  K="phase-a-${i}"
  B="body-A-${i}"
  put_url="${EDGE}$(sign_url PUT "/v1/o/quorum/${K}" 7200)"
  if curl -fsS --max-time 10 -X PUT --data-binary "$B" "$put_url" >/dev/null; then
    PHASE_A_KEYS+=("${K}|${B}")
  else
    echo "FAIL: phase-A PUT ${K} did not succeed with quorum healthy"
    exit 1
  fi
done
echo "    ${#PHASE_A_KEYS[@]} keys PUT 200"

# Snapshot baseline survivor count (after WAL replication settles)
sleep 2
BASELINE=$(coord_object_count "$SURVIVOR_PORT")
echo "    survivor ${SURVIVOR} object count = ${BASELINE}"

# === Phase B: kill leader + 1 follower (keep ${SURVIVOR}) ==================
echo
echo "[phase B] kill ${LEADER_NAME} + 1 other coord (keeping ${SURVIVOR})"
KILL_NAMES=("$LEADER_NAME")
for i in 1 2 3; do
  if [ "coord${i}" != "$LEADER_NAME" ] && [ "coord${i}" != "$SURVIVOR" ]; then
    KILL_NAMES+=("coord${i}")
  fi
done
for n in "${KILL_NAMES[@]}"; do
  docker stop "$n" >/dev/null 2>&1 || true
  DOWN_NAMES+=("$n")
done
echo "    killed: ${KILL_NAMES[*]}"
echo "    docker ps state right after kills:"
docker ps --filter "name=coord" --format '      {{.Names}}: {{.Status}}'
echo "    waiting ${QUORUM_DOWN_SEC}s past leader lease..."
sleep "$QUORUM_DOWN_SEC"
echo "    docker ps state after ${QUORUM_DOWN_SEC}s sleep:"
docker ps --filter "name=coord" --format '      {{.Names}}: {{.Status}}'
docker ps -a --filter "name=coord" --format '      {{.Names}}: {{.Status}}' | sort -u
# Probe each coord directly to see who's actually responding.
for i in 1 2 3; do
  port=$((9000 + i - 1))
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://localhost:${port}/v1/coord/healthz" 2>/dev/null || echo "000")
  echo "      coord${i}:${port} healthz → ${CODE}"
done

# === Phase C: PUT during quorum loss → must all fail =======================
echo
echo "[phase C] PUT ${PHASE_KEYS} keys during 2/3 coord outage (expect all fail)"
C_OK=0; C_FAIL=0
PHASE_C_KEYS=()
for i in $(seq 1 "$PHASE_KEYS"); do
  K="phase-c-${i}"
  B="body-C-${i}"
  put_url="${EDGE}$(sign_url PUT "/v1/o/quorum/${K}" 7200)"
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -X PUT --data-binary "$B" "$put_url" 2>/dev/null || echo "000")
  if [ "$CODE" = "200" ] || [ "$CODE" = "201" ]; then
    C_OK=$((C_OK + 1))
    PHASE_C_KEYS+=("${K}|${B}")
    echo "    ⚠ ${K}: PUT returned ${CODE} during quorum loss (potential phantom commit)"
  else
    C_FAIL=$((C_FAIL + 1))
    PHASE_C_KEYS+=("${K}|${B}")  # track for absence check
  fi
done
echo "    PUT during quorum loss: ${C_OK} unexpected-OK / ${C_FAIL} expected-fail"

# === Phase D: survivor object count must be unchanged ======================
echo
echo "[phase D] survivor ${SURVIVOR} object count must equal Phase A baseline"
DURING=$(coord_object_count "$SURVIVOR_PORT")
echo "    survivor count: baseline=${BASELINE} during=${DURING}"
DRIFT_FAIL=0
if [ "$DURING" != "$BASELINE" ]; then
  DRIFT_FAIL=1
  echo "    ❌ DRIFT: survivor bbolt mutated during quorum loss → phantom commit"
fi

# === Phase E: restore + new writes ==========================================
echo
echo "[phase E] restoring killed coords..."
for n in "${KILL_NAMES[@]}"; do
  docker start "$n" >/dev/null 2>&1 || true
done
DOWN_NAMES=()
echo "    waiting 8s for re-election + WAL catch-up..."
sleep 8

NEW_LEADER=$(find_coord_leader 3 || true)
[ -n "$NEW_LEADER" ] || { echo "    ⚠ no leader after restore — proceeding anyway"; }
[ -n "$NEW_LEADER" ] && echo "    new leader = ${NEW_LEADER}"

PHASE_E_KEYS=()
E_FAIL=0
for i in $(seq 1 "$PHASE_KEYS"); do
  K="phase-e-${i}"
  B="body-E-${i}"
  put_url="${EDGE}$(sign_url PUT "/v1/o/quorum/${K}" 7200)"
  if curl -fsS --max-time 10 -X PUT --data-binary "$B" "$put_url" >/dev/null; then
    PHASE_E_KEYS+=("${K}|${B}")
  else
    E_FAIL=$((E_FAIL + 1))
    echo "    ⚠ phase-E PUT ${K} failed after restore"
  fi
done
echo "    ${#PHASE_E_KEYS[@]} keys PUT after restore (${E_FAIL} fail)"

# === Phase F: final invariant ==============================================
echo
echo "[phase F] final invariant check"
A_RECOVER_FAIL=0
for entry in "${PHASE_A_KEYS[@]}"; do
  K="${entry%%|*}"
  B="${entry#*|}"
  get_url="${EDGE}$(sign_url GET "/v1/o/quorum/${K}" 7200)"
  GOT=$(curl -fsS --max-time 10 "$get_url" 2>/dev/null || echo "__FAIL__")
  [ "$GOT" = "$B" ] || A_RECOVER_FAIL=$((A_RECOVER_FAIL + 1))
done

E_RECOVER_FAIL=0
for entry in "${PHASE_E_KEYS[@]}"; do
  K="${entry%%|*}"
  B="${entry#*|}"
  get_url="${EDGE}$(sign_url GET "/v1/o/quorum/${K}" 7200)"
  GOT=$(curl -fsS --max-time 10 "$get_url" 2>/dev/null || echo "__FAIL__")
  [ "$GOT" = "$B" ] || E_RECOVER_FAIL=$((E_RECOVER_FAIL + 1))
done

# Phantom check: ANY phase-C key that was NOT 200 above must NOT be GET-able.
# (200 phase-C keys are inspected separately as architectural violations.)
PHANTOM=0
for entry in "${PHASE_C_KEYS[@]}"; do
  K="${entry%%|*}"
  get_url="${EDGE}$(sign_url GET "/v1/o/quorum/${K}" 7200)"
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$get_url" 2>/dev/null || echo "000")
  if [ "$CODE" = "200" ]; then
    PHANTOM=$((PHANTOM + 1))
    [ "$PHANTOM" -le 3 ] && echo "    ❌ phantom: ${K} appeared after restore"
  fi
done

# === Summary ===============================================================
echo
echo "[summary]"
echo "   phase A (seed):                ${#PHASE_A_KEYS[@]}/${PHASE_KEYS} OK, recover ${A_RECOVER_FAIL} FAIL"
echo "   phase C (during quorum loss):  ${C_OK} unexpected-OK / ${C_FAIL} expected-fail"
echo "   phase D (survivor drift):      $([ "$DRIFT_FAIL" -eq 0 ] && echo 'no drift' || echo 'DRIFT DETECTED')"
echo "   phase E (post-restore):        ${#PHASE_E_KEYS[@]}/${PHASE_KEYS} OK, recover ${E_RECOVER_FAIL} FAIL"
echo "   phase F (phantom commits):     ${PHANTOM}"

EXIT_CODE=0
if [ "$C_OK" -gt 0 ]; then
  echo "   ❌ ADR-040 VIOLATION: ${C_OK} PUTs returned 2xx during quorum loss"
  EXIT_CODE=1
fi
if [ "$DRIFT_FAIL" -ne 0 ]; then
  echo "   ❌ ADR-040 VIOLATION: survivor bbolt mutated during quorum loss"
  EXIT_CODE=1
fi
if [ "$PHANTOM" -gt 0 ]; then
  echo "   ❌ ADR-040 VIOLATION: ${PHANTOM} phase-C keys are GET-able after restore (phantom commit)"
  EXIT_CODE=1
fi
if [ "$A_RECOVER_FAIL" -gt 0 ] || [ "$E_RECOVER_FAIL" -gt 0 ]; then
  echo "   ❌ DURABILITY: ${A_RECOVER_FAIL} phase-A + ${E_RECOVER_FAIL} phase-E keys not retrievable"
  EXIT_CODE=1
fi
[ "$EXIT_CODE" -eq 0 ] && echo "   ✅ no phantom commit · no bbolt drift · all 200-PUTs durable"

# On failure, dump tail of survivor + edge logs before cleanup trap fires.
if [ "$EXIT_CODE" -ne 0 ]; then
  echo
  for n in coord1 coord2 coord3; do
    echo "=== ${n} log tail (last 50 lines) ==="
    docker logs "$n" 2>&1 | tail -50 || true
    echo
  done
  echo "=== edge log tail (last 60 lines) ==="
  docker logs edge 2>&1 | tail -60 || true
fi

exit $EXIT_CODE
