#!/usr/bin/env bash
# chaos-mixed.sh — DN flap + coord flap simultaneously. Catches regressions
# that show up only when multiple subsystems are unstable at once.
#
# What it tests:
#   - quorum-write completion when both DN AND coord topologies are
#     shifting under load (R/2+1 ack on chunk side; coord HA failover
#     on meta side)
#   - CoordClient leader-redirect transparency under DN-side noise
#   - Final invariant: every 200-PUT survives both subsystems' churn
#
# Self-contained: 4 DN + 3-coord HA + edge. Quorum on DN: 2 alive needed
# (R=3, write quorum 2). Quorum on coord: 2 alive needed (3-coord, 2/3).
# So we kill at most 1 DN AND 1 coord simultaneously — never below quorum
# on either dimension.
#
# Usage:
#   ./scripts/chaos-mixed.sh                       # 90s
#   ./scripts/chaos-mixed.sh --duration 180 --interval 8
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=90
INTERVAL=10        # seconds between churn events
DOWNTIME=4         # seconds dead before restart
GET_RATE=3
PUT_RATE=1
GET_FAIL_PCT_MAX=10  # higher tolerance than single-subsystem tests

usage() {
  cat <<'EOF'
chaos-mixed.sh — DN flap + coord flap simultaneously, asserts every
200-PUT durable + GET fail rate within tolerance.

Flags:
  --duration <s>      total runtime  (default 90)
  --interval <s>      seconds between churn events (default 10)
  --downtime <s>      seconds dead before restart (default 4)
  --get-rate <n>      GET per second (default 3)
  --put-rate <n>      PUT per second (default 1)
  --get-fail-max <%>  soft GET-fail threshold (default 10)
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
DN_NAMES=(dn1 dn2 dn3 dn4)   # 4 DN so we can kill 1 + still have R=3 alive

need curl; need jq; need docker; need python3

echo "🌪  chaos-mixed (DN + coord flap)"
echo "   duration:  ${DURATION}s, churn every ${INTERVAL}s, downtime ${DOWNTIME}s"
echo "   GET rate:  ${GET_RATE}/s, PUT rate: ${PUT_RATE}/s"
echo "   topology:  4 DN + 3 coord HA + edge"
echo

echo "[setup] tearing down any prior cluster..."
./scripts/down.sh >/dev/null 2>&1 || true

rebuild_images
docker network create $NET 2>/dev/null || true

# 4 DN — start_dns helper handles 1..N
start_dns 4

# Override COORD_DNS to include all 4
COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  bash -c "
    . scripts/lib/common.sh
    . scripts/lib/cluster.sh
    NET='$NET'
    for i in 1 2 3; do
      start_coord coord\${i} \$((9000 + i - 1)) \
        '$PEERS_INTERNAL' \"http://coord\${i}:9000\" \
        '/var/lib/kvfs-coord/coord.wal' '1'
    done
  "

sleep 5
LEADER0=$(find_coord_leader 3 || true)
[ -n "$LEADER0" ] || { echo "FAIL: no initial leader"; exit 1; }
echo "    initial leader = $LEADER0"

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  bash -c "
    . scripts/lib/common.sh
    . scripts/lib/cluster.sh
    NET='$NET' SECRET='$SECRET'
    start_edge edge 8000 'http://coord1:9000'
  "
wait_healthz "${EDGE}/healthz"

DOWN_DNS=()
DOWN_COORDS=()
trap '
  for n in "${DOWN_DNS[@]}" "${DOWN_COORDS[@]}"; do docker start "$n" >/dev/null 2>&1 || true; done
  rm -f "$TMP_BODY" "$TMP_GOT" "$PUT_LOG" 2>/dev/null || true
  ./scripts/down.sh >/dev/null 2>&1 || true
' EXIT INT TERM

# Seed
TMP_BODY=$(mktemp); TMP_GOT=$(mktemp); PUT_LOG=$(mktemp)
SEED_BUCKET="chaos"; SEED_KEY="seed.bin"
head -c $((64 * 1024)) /dev/urandom > "$TMP_BODY"
SEED_SHA=$(sha256sum "$TMP_BODY" | awk '{print $1}')
seed_url="${EDGE}$(sign_url PUT "/v1/o/${SEED_BUCKET}/${SEED_KEY}" 7200)"
curl -fsS -X PUT "$seed_url" --data-binary "@${TMP_BODY}" >/dev/null
echo "[seed] PUT seed.bin sha=${SEED_SHA:0:16}.."
echo

# Counters
GETS=0; GET_OK=0; GET_FAIL=0
PUTS=0; PUT_OK=0; PUT_FAIL=0
DN_KILLS=0; COORD_KILLS=0

GET_INTERVAL=$(python3 -c "print(1.0/${GET_RATE})")
PUT_EVERY=$((GET_RATE / PUT_RATE))
[ "$PUT_EVERY" -lt 1 ] && PUT_EVERY=1

START=$(date +%s)
NEXT_CHURN=$((START + INTERVAL))
END=$((START + DURATION))

declare -A DOWN_DN_AT
declare -A DOWN_COORD_AT

restore_due() {
  local now="$1"
  for name in "${!DOWN_DN_AT[@]}"; do
    if [ "$now" -ge "${DOWN_DN_AT[$name]}" ]; then
      docker start "$name" >/dev/null 2>&1 || true
      unset "DOWN_DN_AT[$name]"
      DOWN_DNS=("${DOWN_DNS[@]/$name}")
      printf "       [%4ds] ↑ DN restored %s\n" "$((now - START))" "$name"
    fi
  done
  for name in "${!DOWN_COORD_AT[@]}"; do
    if [ "$now" -ge "${DOWN_COORD_AT[$name]}" ]; then
      docker start "$name" >/dev/null 2>&1 || true
      unset "DOWN_COORD_AT[$name]"
      DOWN_COORDS=("${DOWN_COORDS[@]/$name}")
      printf "       [%4ds] ↑ coord restored %s\n" "$((now - START))" "$name"
    fi
  done
}

echo "[chaos] running for ${DURATION}s..."
ITER=0
while :; do
  NOW=$(date +%s)
  if [ "$NOW" -ge "$END" ]; then break; fi
  restore_due "$NOW"

  # Churn: kill 1 DN AND 1 coord simultaneously (each within their quorum-safe set)
  if [ "$NOW" -ge "$NEXT_CHURN" ]; then
    # Pick DN candidate (alive, can lose 1 of 4 → R=3 still served)
    dn_cands=()
    for n in "${DN_NAMES[@]}"; do [ -z "${DOWN_DN_AT[$n]+x}" ] && dn_cands+=("$n"); done
    if [ "${#dn_cands[@]}" -ge 4 ]; then  # only kill if all 4 alive (keep ≥3)
      victim_dn="${dn_cands[$((RANDOM % ${#dn_cands[@]}))]}"
      docker stop "$victim_dn" >/dev/null 2>&1 || true
      DOWN_DN_AT["$victim_dn"]=$((NOW + DOWNTIME))
      DOWN_DNS+=("$victim_dn")
      DN_KILLS=$((DN_KILLS + 1))
      printf "       [%4ds] ↓ DN kill    %s\n" "$((NOW - START))" "$victim_dn"
    fi

    # Pick coord candidate (alive, must keep ≥2 of 3 for quorum)
    co_cands=()
    for n in "${COORD_NAMES[@]}"; do [ -z "${DOWN_COORD_AT[$n]+x}" ] && co_cands+=("$n"); done
    if [ "${#co_cands[@]}" -ge 3 ]; then
      victim_co="${co_cands[$((RANDOM % ${#co_cands[@]}))]}"
      docker stop "$victim_co" >/dev/null 2>&1 || true
      DOWN_COORD_AT["$victim_co"]=$((NOW + DOWNTIME))
      DOWN_COORDS+=("$victim_co")
      COORD_KILLS=$((COORD_KILLS + 1))
      printf "       [%4ds] ↓ coord kill %s\n" "$((NOW - START))" "$victim_co"
    fi

    NEXT_CHURN=$((NOW + INTERVAL))
  fi

  # GET seed
  get_url="${EDGE}$(sign_url GET "/v1/o/${SEED_BUCKET}/${SEED_KEY}" 7200)"
  if curl -fsS --max-time 10 "$get_url" -o "$TMP_GOT" 2>/dev/null; then
    GOT_SHA=$(sha256sum "$TMP_GOT" | awk '{print $1}')
    [ "$GOT_SHA" = "$SEED_SHA" ] && GET_OK=$((GET_OK + 1)) || GET_FAIL=$((GET_FAIL + 1))
  else
    GET_FAIL=$((GET_FAIL + 1))
  fi
  GETS=$((GETS + 1))

  # PUT new
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

echo
echo "[verify] restoring all..."
for n in "${!DOWN_DN_AT[@]}" "${!DOWN_COORD_AT[@]}"; do
  docker start "$n" >/dev/null 2>&1 || true
done
DOWN_DN_AT=(); DOWN_COORD_AT=()
DOWN_DNS=(); DOWN_COORDS=()

echo "[verify] waiting 8s for catch-up..."
sleep 8

# Final verification
FINAL_OK=0; FINAL_FAIL=0
TOTAL_PUTS=$(wc -l < "$PUT_LOG" | tr -d ' ')
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

GET_FAIL_PCT=$(python3 -c "print(round(${GET_FAIL}*100.0/max(1,${GETS}), 2))")
PUT_FAIL_PCT=$(python3 -c "print(round(${PUT_FAIL}*100.0/max(1,${PUTS}), 2))")

echo
echo "[summary]"
echo "   duration:        ${DURATION}s"
echo "   DN kills:        ${DN_KILLS}"
echo "   coord kills:     ${COORD_KILLS}"
echo "   GETs:            ${GETS}  (OK ${GET_OK} / FAIL ${GET_FAIL} = ${GET_FAIL_PCT}%)"
echo "   PUTs:            ${PUTS}  (OK ${PUT_OK} / FAIL ${PUT_FAIL} = ${PUT_FAIL_PCT}%)"
echo "   final verify:    ${FINAL_OK} OK / ${FINAL_FAIL} FAIL on ${TOTAL_PUTS} 200-PUTs"

EXIT_CODE=0
HARD_BAR=$(python3 -c "print(int(${GET_FAIL_PCT_MAX}*100))")
GET_PCT_X100=$(python3 -c "print(int(${GET_FAIL_PCT}*100))")

if [ "$FINAL_FAIL" -gt 0 ]; then
  echo "   ❌ INVARIANT BROKEN: ${FINAL_FAIL} 200-PUTs lost across mixed churn"
  EXIT_CODE=1
fi
if [ "$GET_PCT_X100" -gt "$HARD_BAR" ]; then
  echo "   ⚠ GET fail rate ${GET_FAIL_PCT}% > threshold ${GET_FAIL_PCT_MAX}%"
  EXIT_CODE=1
fi
if [ "$EXIT_CODE" -eq 0 ]; then
  echo "   ✅ ${DN_KILLS} DN + ${COORD_KILLS} coord kills, all 200-PUTs durable"
fi

# Post-mortem: if any 200-PUT was lost, inspect each coord's object count
# to localize the inconsistency (simplified Raft: stale follower can win
# election without log-up-to-date check; missed entries don't catch up
# on restart since walHook is push-only, not pull-on-rejoin).
if [ "$FINAL_FAIL" -gt 0 ]; then
  echo
  echo "[post-mortem] per-coord object count (looking for divergence):"
  for i in 1 2 3; do
    port=$((9000 + i - 1))
    cnt=$(curl -fsS --max-time 5 "http://localhost:${port}/v1/coord/admin/objects" 2>/dev/null | jq -r 'length' 2>/dev/null || echo "ERR")
    role=$(curl -fsS --max-time 2 "http://localhost:${port}/v1/coord/healthz" 2>/dev/null | jq -r '.role' 2>/dev/null || echo "?")
    echo "    coord${i} (role=${role}): ${cnt} objects"
  done
  echo
  echo "[known limitation] kvfs's simplified Elector lacks Raft's"
  echo "  'log-up-to-date' invariant during voting (§5.4.1 of the Raft"
  echo "  paper). A coord that was dead during a commit window can later"
  echo "  win an election with a stale log if its term is incremented"
  echo "  first. Plus coord follower restart does not refetch missed"
  echo "  entries (walHook is push-only). FOLLOWUP P8-06 tracks the fix."
fi
exit $EXIT_CODE
