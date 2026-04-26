#!/usr/bin/env bash
# chaos-dn-killer.sh — periodically stop a random DN for a few seconds, then
# restart it. While doing so, hammer GET on a seed object and verify each
# response matches the original sha256.
#
# Goal: catch regressions in quorum reads, EC reconstruction, rebalance
# /apply paths, and edge timeout handling under flapping-DN conditions.
#
# Prerequisites:
#   - Cluster running (./scripts/up.sh or ./scripts/demo-eta.sh tail state)
#   - At least 3 DNs so quorum (2-of-3) survives 1 kill
#   - jq optional (only used to pretty-print failures)
#
# Usage:
#   ./scripts/chaos-dn-killer.sh                       # 60s, kill every 10s
#   ./scripts/chaos-dn-killer.sh --duration 300 --interval 5 --get-rate 5
#   ./scripts/chaos-dn-killer.sh --dns "dn1 dn2 dn3 dn4"
set -euo pipefail

cd "$(dirname "$0")/.."

DURATION=60       # total runtime (s)
INTERVAL=10       # seconds between kill events
GET_RATE=2        # GET requests per second
DOWNTIME=3        # seconds a killed DN stays down before restart
DNS=""            # auto-detect via docker ps if empty
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="chaos"
KEY="seed.bin"
SIZE=$((64 * 1024))   # 64 KiB seed

usage() {
  cat <<'EOF'
chaos-dn-killer.sh — periodically stop a random DN, restart it, and verify
GET still returns the correct sha256 throughout. Catches regressions in
quorum reads, EC reconstruction, and edge timeout handling.

Prereq: cluster running (./scripts/up.sh or any demo tail) with >= 3 DNs.

Flags:
  --duration <s>   total runtime (default 60)
  --interval <s>   seconds between kill events (default 10)
  --get-rate <n>   GET requests per second (default 2)
  --downtime <s>   seconds a killed DN stays down (default 3)
  --dns "<list>"   DN container names (default: auto-detect via docker ps)

Examples:
  ./scripts/chaos-dn-killer.sh
  ./scripts/chaos-dn-killer.sh --duration 300 --interval 5 --get-rate 5
  ./scripts/chaos-dn-killer.sh --dns "dn1 dn2 dn3 dn4"
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --duration)  DURATION="$2"; shift 2 ;;
    --interval)  INTERVAL="$2"; shift 2 ;;
    --get-rate)  GET_RATE="$2"; shift 2 ;;
    --downtime)  DOWNTIME="$2"; shift 2 ;;
    --dns)       DNS="$2"; shift 2 ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "unknown flag: $1"; usage; exit 2 ;;
  esac
done

. "$(dirname "$0")/lib/common.sh"

need curl; need python3; need docker

if [ -z "$DNS" ]; then
  DNS=$(docker ps --format '{{.Names}}' | grep -E '^dn[0-9]+$' | sort | tr '\n' ' ')
fi
if [ -z "$DNS" ]; then
  echo "no DN containers found (expected dn1, dn2, ...). Bring cluster up first."
  exit 1
fi
read -ra DN_ARR <<<"$DNS"
if [ "${#DN_ARR[@]}" -lt 3 ]; then
  echo "need >= 3 DNs for quorum chaos test, found ${#DN_ARR[@]}: ${DN_ARR[*]}"
  exit 1
fi

if ! curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then
  echo "edge not reachable at ${EDGE}"
  exit 1
fi

echo "🌪  chaos-dn-killer"
echo "   duration:  ${DURATION}s"
echo "   interval:  every ${INTERVAL}s, kill 1 random DN"
echo "   downtime:  ${DOWNTIME}s per kill"
echo "   get-rate:  ${GET_RATE}/s on chaos/seed.bin"
echo "   DN pool:   ${DN_ARR[*]}"
echo


TMP_BODY=$(mktemp)
TMP_GOT=$(mktemp)
trap 'rm -f "$TMP_BODY" "$TMP_GOT"' EXIT

echo "[seed] generating ${SIZE}-byte body and PUT"
head -c "$SIZE" /dev/urandom > "$TMP_BODY"
EXPECTED_SHA=$(sha256sum "$TMP_BODY" | awk '{print $1}')

put_url="${EDGE}$(sign_url PUT "/v1/o/${BUCKET}/${KEY}" 7200)"
curl -fsS -X PUT "$put_url" -H "Content-Type: application/octet-stream" \
  --data-binary "@${TMP_BODY}" >/dev/null
echo "       sha256 = ${EXPECTED_SHA:0:32}.."

# Counters
GETS=0; OK=0; FAIL=0; KILLS=0
GET_INTERVAL=$(python3 -c "print(1.0/$GET_RATE)")

START=$(date +%s)
NEXT_KILL=$((START + INTERVAL))
END=$((START + DURATION))

# Restart any DN we left stopped. Tracked by name + restore_at_epoch.
declare -A DOWN_DNS    # name -> restore_epoch

restore_due() {
  local now="$1"
  for name in "${!DOWN_DNS[@]}"; do
    if [ "$now" -ge "${DOWN_DNS[$name]}" ]; then
      docker start "$name" >/dev/null 2>&1 || true
      unset "DOWN_DNS[$name]"
      printf "       [%4ds] ↑ restored %s\n" "$((now - START))" "$name"
    fi
  done
}

restore_all() {
  for name in "${!DOWN_DNS[@]}"; do
    docker start "$name" >/dev/null 2>&1 || true
  done
  DOWN_DNS=()
}
trap 'restore_all; rm -f "$TMP_BODY" "$TMP_GOT"' EXIT

echo
echo "[chaos] running for ${DURATION}s..."
while :; do
  NOW=$(date +%s)
  if [ "$NOW" -ge "$END" ]; then break; fi
  restore_due "$NOW"

  # Fire a kill if due
  if [ "$NOW" -ge "$NEXT_KILL" ]; then
    # pick a candidate that isn't already down
    candidates=()
    for name in "${DN_ARR[@]}"; do
      if [ -z "${DOWN_DNS[$name]+x}" ]; then
        candidates+=("$name")
      fi
    done
    # Don't kill below quorum (= R/2+1; assume R=3 → keep >= 2 alive)
    alive=${#candidates[@]}
    if [ "$alive" -gt 2 ]; then
      victim="${candidates[$((RANDOM % alive))]}"
      docker stop "$victim" >/dev/null 2>&1 || true
      DOWN_DNS["$victim"]=$((NOW + DOWNTIME))
      KILLS=$((KILLS + 1))
      printf "       [%4ds] ↓ killed   %s (restore in %ds)\n" \
        "$((NOW - START))" "$victim" "$DOWNTIME"
    fi
    NEXT_KILL=$((NOW + INTERVAL))
  fi

  # GET request
  get_url="${EDGE}$(sign_url GET "/v1/o/${BUCKET}/${KEY}" 7200)"
  if curl -fsS --max-time 5 "$get_url" -o "$TMP_GOT" 2>/dev/null; then
    GOT_SHA=$(sha256sum "$TMP_GOT" | awk '{print $1}')
    if [ "$GOT_SHA" = "$EXPECTED_SHA" ]; then
      OK=$((OK + 1))
    else
      FAIL=$((FAIL + 1))
      printf "       [%4ds] ⚠ sha mismatch (got %s..)\n" \
        "$((NOW - START))" "${GOT_SHA:0:16}"
    fi
  else
    FAIL=$((FAIL + 1))
    printf "       [%4ds] ⚠ GET failed\n" "$((NOW - START))"
  fi
  GETS=$((GETS + 1))
  sleep "$GET_INTERVAL"
done

restore_all
echo
echo "[summary]"
echo "   GET total:  $GETS  (rate ~$(python3 -c "print(round($GETS/$DURATION, 2))")/s)"
echo "   OK:         $OK"
echo "   FAIL:       $FAIL"
echo "   kills:      $KILLS"
if [ "$FAIL" -eq 0 ]; then
  echo "   ✅ no read failures during ${KILLS} DN kills"
  exit 0
else
  echo "   ❌ ${FAIL} failures observed — investigate edge timeouts / quorum config"
  exit 1
fi
