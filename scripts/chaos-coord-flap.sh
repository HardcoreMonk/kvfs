#!/usr/bin/env bash
# chaos-coord-flap.sh — periodically kill+restart a random coord (keeping
# quorum), hammer PUT+GET, verify every 200-PUT survives the run.
#
# Goal: catch regressions in:
#   - ADR-038 coord HA via Raft   (election after leader kill)
#   - ADR-039 coord WAL replication (pre-failover writes survive)
#   - ADR-040 transactional commit (no phantom commits)
#   - P7-08 CANDIDATE retry        (transparent recovery within election)
#
# The invariant we assert: any PUT that returned 200 MUST be GET-able with
# matching body at the end of the run, regardless of how many coords were
# bounced in between. Violations indicate phantom commit (ADR-040 broken)
# or WAL replication gap (ADR-039 broken).
#
# Self-contained: tears down any prior cluster, brings up its own
# 3-coord HA + 3-DN + edge, runs chaos, tears down on exit.
#
# Usage:
#   ./scripts/chaos-coord-flap.sh                       # 90s, kill every 12s
#   ./scripts/chaos-coord-flap.sh --duration 300 --interval 8
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=90
INTERVAL=12
DOWNTIME=4
GET_RATE=4   # GET per second
PUT_RATE=1   # PUT per second
GET_FAIL_PCT_MAX=5  # soft threshold

usage() {
  cat <<'EOF'
chaos-coord-flap.sh — kill/restart random coord on a schedule, hammer
PUT+GET, verify every 200-PUT is GET-able at end. Self-contained: brings
up 3-coord HA + 3-DN + edge, tears down on exit.

Flags:
  --duration <s>     total runtime         (default 90)
  --interval <s>     seconds between kills (default 12)
  --downtime <s>     seconds dead before restart (default 4)
  --get-rate <n>     GET per second        (default 4)
  --put-rate <n>     PUT per second        (default 1)
  --get-fail-max <%> soft GET-fail threshold (default 5)

Hard PASS criteria (always asserted):
  - every PUT that returned 200 is GET-able at end with matching body

Soft PASS criteria (warning only, configurable):
  - GET fail rate <= --get-fail-max %  (defaults to 5%)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --duration)     DURATION="$2"; shift 2 ;;
    --interval)     INTERVAL="$2"; shift 2 ;;
    --downtime)     DOWNTIME="$2"; shift 2 ;;
    --get-rate)     GET_RATE="$2"; shift 2 ;;
    --put-rate)     PUT_RATE="$2"; shift 2 ;;
    --get-fail-max) GET_FAIL_PCT_MAX="$2"; shift 2 ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "unknown flag: $1"; usage; exit 2 ;;
  esac
done

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"
COORD_NAMES=(coord1 coord2 coord3)

need curl; need jq; need docker; need python3

echo "🌪  chaos-coord-flap"
echo "   duration:  ${DURATION}s"
echo "   interval:  kill every ${INTERVAL}s, downtime ${DOWNTIME}s per kill"
echo "   GET rate:  ${GET_RATE}/s"
echo "   PUT rate:  ${PUT_RATE}/s"
echo "   target:    ADR-038 (HA) · ADR-039 (WAL repl) · ADR-040 (txn commit)"
echo

# === Setup ==================================================================
echo "[setup] tearing down any prior cluster..."
./scripts/down.sh >/dev/null 2>&1 || true

rebuild_images
docker network create $NET 2>/dev/null || true

echo "[setup] starting 3 DNs..."
start_dns 3

echo "[setup] starting 3 coords (Raft + WAL repl + transactional commit)..."
for i in 1 2 3; do
  start_coord "coord${i}" $((9000 + i - 1)) \
    "$PEERS_INTERNAL" "http://coord${i}:9000" \
    "/var/lib/kvfs-coord/coord.wal" "1"
done

echo "[setup] waiting for initial election (~5s)..."
sleep 5
LEADER0=$(find_coord_leader 3 || true)
[ -n "$LEADER0" ] || { echo "FAIL: no initial leader"; for i in 1 2 3; do docker logs "coord${i}" 2>&1 | tail -8; done; exit 1; }
echo "    initial leader = $LEADER0"

echo "[setup] starting edge (EDGE_COORD_URL=http://coord1:9000)..."
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"
echo

# Cleanup trap — restore coords, then full down.sh
DOWN_COORDS=()
trap '
  for n in "${DOWN_COORDS[@]}"; do docker start "$n" >/dev/null 2>&1 || true; done
  rm -f "$TMP_BODY" "$TMP_GOT" 2>/dev/null || true
  ./scripts/down.sh >/dev/null 2>&1 || true
' EXIT INT TERM

# === Seed ===================================================================
SEED_BUCKET="chaos"
SEED_KEY="seed.bin"
SEED_SIZE=$((64 * 1024))
TMP_BODY=$(mktemp)
TMP_GOT=$(mktemp)
PUT_LOG=$(mktemp)

head -c "$SEED_SIZE" /dev/urandom > "$TMP_BODY"
SEED_SHA=$(sha256sum "$TMP_BODY" | awk '{print $1}')
seed_url="${EDGE}$(sign_url PUT "/v1/o/${SEED_BUCKET}/${SEED_KEY}" 7200)"
curl -fsS -X PUT "$seed_url" --data-binary "@${TMP_BODY}" >/dev/null
echo "[seed] PUT ${SEED_BUCKET}/${SEED_KEY} (${SEED_SIZE}B), sha=${SEED_SHA:0:16}.."
echo

# === Chaos loop =============================================================
GETS=0; GET_OK=0; GET_FAIL=0
PUTS=0; PUT_OK=0; PUT_FAIL=0
KILLS=0

GET_INTERVAL=$(python3 -c "print(1.0/${GET_RATE})")
PUT_EVERY=$((GET_RATE / PUT_RATE))
[ "$PUT_EVERY" -lt 1 ] && PUT_EVERY=1

START=$(date +%s)
NEXT_KILL=$((START + INTERVAL))
END=$((START + DURATION))

declare -A DOWN_AT  # name -> restore_epoch

restore_due() {
  local now="$1"
  for name in "${!DOWN_AT[@]}"; do
    if [ "$now" -ge "${DOWN_AT[$name]}" ]; then
      docker start "$name" >/dev/null 2>&1 || true
      unset "DOWN_AT[$name]"
      DOWN_COORDS=("${DOWN_COORDS[@]/$name}")
      printf "       [%4ds] ↑ restored %s\n" "$((now - START))" "$name"
    fi
  done
}

echo "[chaos] running for ${DURATION}s..."
ITER=0
while :; do
  NOW=$(date +%s)
  if [ "$NOW" -ge "$END" ]; then break; fi
  restore_due "$NOW"

  # --- kill scheduler ---
  if [ "$NOW" -ge "$NEXT_KILL" ]; then
    candidates=()
    for name in "${COORD_NAMES[@]}"; do
      [ -z "${DOWN_AT[$name]+x}" ] && candidates+=("$name")
    done
    alive=${#candidates[@]}
    # need >= 2 alive AFTER the kill (quorum); only kill if >= 3 alive now
    if [ "$alive" -ge 3 ]; then
      victim="${candidates[$((RANDOM % alive))]}"
      docker stop "$victim" >/dev/null 2>&1 || true
      DOWN_AT["$victim"]=$((NOW + DOWNTIME))
      DOWN_COORDS+=("$victim")
      KILLS=$((KILLS + 1))
      printf "       [%4ds] ↓ killed   %s (restore in %ds)\n" \
        "$((NOW - START))" "$victim" "$DOWNTIME"
    fi
    NEXT_KILL=$((NOW + INTERVAL))
  fi

  # --- GET seed ---
  get_url="${EDGE}$(sign_url GET "/v1/o/${SEED_BUCKET}/${SEED_KEY}" 7200)"
  if curl -fsS --max-time 10 "$get_url" -o "$TMP_GOT" 2>/dev/null; then
    GOT_SHA=$(sha256sum "$TMP_GOT" | awk '{print $1}')
    if [ "$GOT_SHA" = "$SEED_SHA" ]; then
      GET_OK=$((GET_OK + 1))
    else
      GET_FAIL=$((GET_FAIL + 1))
      printf "       [%4ds] ⚠ seed sha mismatch (got %s..)\n" \
        "$((NOW - START))" "${GOT_SHA:0:16}"
    fi
  else
    GET_FAIL=$((GET_FAIL + 1))
  fi
  GETS=$((GETS + 1))

  # --- PUT new (every PUT_EVERY iterations) ---
  if (( ITER % PUT_EVERY == 0 )); then
    PUT_KEY="put-$((PUTS + 1))"
    PUT_BODY="payload-${PUT_KEY}-$(date +%s%N)"
    put_url="${EDGE}$(sign_url PUT "/v1/o/chaos/${PUT_KEY}" 7200)"
    if curl -fsS --max-time 10 -X PUT --data-binary "$PUT_BODY" "$put_url" >/dev/null 2>&1; then
      PUT_OK=$((PUT_OK + 1))
      echo "${PUT_KEY}|${PUT_BODY}" >> "$PUT_LOG"
    else
      PUT_FAIL=$((PUT_FAIL + 1))
    fi
    PUTS=$((PUTS + 1))
  fi

  ITER=$((ITER + 1))
  sleep "$GET_INTERVAL"
done

# === Restore + settle =======================================================
echo
echo "[verify] restoring all coords..."
for name in "${!DOWN_AT[@]}"; do
  docker start "$name" >/dev/null 2>&1 || true
done
DOWN_AT=()
DOWN_COORDS=()

echo "[verify] waiting 6s for stragglers to catch up via WAL replication..."
sleep 6

# === Final invariant check ==================================================
FINAL_OK=0
FINAL_FAIL=0
TOTAL_PUT_LOG=$(wc -l < "$PUT_LOG" | tr -d ' ')

while IFS='|' read -r K B; do
  get_url="${EDGE}$(sign_url GET "/v1/o/chaos/${K}" 7200)"
  GOT=$(curl -fsS --max-time 10 "$get_url" 2>/dev/null || echo "__FETCH_FAIL__")
  if [ "$GOT" = "$B" ]; then
    FINAL_OK=$((FINAL_OK + 1))
  else
    FINAL_FAIL=$((FINAL_FAIL + 1))
    if [ "$FINAL_FAIL" -le 3 ]; then
      printf "       ⚠ key=%s expected=%s.. got=%s..\n" \
        "$K" "${B:0:24}" "${GOT:0:24}"
    fi
  fi
done < "$PUT_LOG"
rm -f "$PUT_LOG"

# === Summary ===============================================================
GET_FAIL_PCT=$(python3 -c "print(round(${GET_FAIL}*100.0/max(1,${GETS}), 2))")
PUT_FAIL_PCT=$(python3 -c "print(round(${PUT_FAIL}*100.0/max(1,${PUTS}), 2))")

echo
echo "[summary]"
echo "   duration:     ${DURATION}s, ${KILLS} coord kills"
echo "   GETs:         ${GETS}  (OK ${GET_OK} / FAIL ${GET_FAIL} = ${GET_FAIL_PCT}%)"
echo "   PUTs:         ${PUTS}  (OK ${PUT_OK} / FAIL ${PUT_FAIL} = ${PUT_FAIL_PCT}%)"
echo "   final verify: ${FINAL_OK} OK / ${FINAL_FAIL} FAIL on ${TOTAL_PUT_LOG} 200-PUTs"

EXIT_CODE=0
HARD_BAR=$(python3 -c "print(int(${GET_FAIL_PCT_MAX}*100))")
GET_PCT_X100=$(python3 -c "print(int(${GET_FAIL_PCT}*100))")

if [ "$FINAL_FAIL" -gt 0 ]; then
  echo "   ❌ ARCHITECTURAL INVARIANT BROKEN: ${FINAL_FAIL} 200-PUTs lost across coord flap"
  echo "      → ADR-040 transactional commit OR ADR-039 WAL replication regression"
  EXIT_CODE=1
fi
if [ "$GET_PCT_X100" -gt "$HARD_BAR" ]; then
  echo "   ⚠ GET fail rate ${GET_FAIL_PCT}% > threshold ${GET_FAIL_PCT_MAX}%"
  echo "      → likely CoordClient retry / failover transparency regression"
  EXIT_CODE=1
fi
if [ "$EXIT_CODE" -eq 0 ]; then
  echo "   ✅ all ${TOTAL_PUT_LOG} 200-PUTs survived ${KILLS} coord kills"
  echo "   ✅ GET fail rate ${GET_FAIL_PCT}% within ${GET_FAIL_PCT_MAX}% threshold"
fi

exit $EXIT_CODE
