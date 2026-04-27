#!/usr/bin/env bash
# demo-anti-entropy-throttle.sh — P8-12 (ADR-059): max_repairs throttle
# + per-stripe precision in EC outcomes.
#
# Stages:
#   1. 3-DN cluster + coord + edge.
#   2. PUT 6 distinct objects (so we have 6 distinct missing candidates).
#   3. rm 5 chunk files from dn1.
#   4. anti-entropy/repair?max_repairs=2 → exactly 2 repaired,
#      3 marked throttled (Skipped with mode="throttled").
#   5. anti-entropy/repair (no cap) → remaining 3 repaired.
#   6. anti-entropy run → all root_match=true.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy throttle + per-stripe precision demo (P8-12, ADR-059) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

start_dns 3
COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD_URL}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> stage 1: PUT 6 distinct objects"
for i in 1 2 3 4 5 6; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/throttle/obj${i}" 60)"
  curl -fs -X PUT --data-binary "throttle-body-${i}-distinguishable-payload" "$put_url" >/dev/null
done
echo "    seeded 6 objects"
echo

echo "==> stage 2: rm 5 chunk files from dn1"
N_RM=0
curl -fs "${COORD_URL}/v1/coord/admin/objects" \
  | jq -r '.[].chunks[].chunk_id' \
  | while read -r CID; do
      DN_PATH="/var/lib/kvfs-dn/chunks/${CID:0:2}/${CID:2}"
      if docker exec dn1 rm -f "$DN_PATH" 2>/dev/null; then
        N_RM=$((N_RM + 1))
        echo "    removed ${CID:0:16}.. from dn1"
      fi
      [ "$N_RM" -ge 5 ] && break
    done
echo

echo "==> stage 3: anti-entropy/repair?max_repairs=2 → 2 done + remainder throttled"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?max_repairs=2")
echo "$REP" | jq -c '{
  repaired: [(.repairs // [])[] | select(.ok)] | length,
  throttled_skipped: [(.skipped // [])[] | select(.mode=="throttled")] | length
}'
REPAIRED=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.ok)] | length')
THROTTLED=$(echo "$REP" | jq -r '[(.skipped // [])[] | select(.mode=="throttled")] | length')
[ "$REPAIRED" = "2" ] || { echo "FAIL: expected 2 repaired, got ${REPAIRED}"; exit 1; }
[ "$THROTTLED" -ge 3 ] || { echo "FAIL: expected >= 3 throttled, got ${THROTTLED}"; exit 1; }
echo "    ✓ throttle stopped at ${REPAIRED}, ${THROTTLED} marked throttled"
echo

echo "==> stage 4: anti-entropy run shows dn1 still missing the rest"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/run")
DN1_MISS=$(echo "$REP" | jq -r '.dns[] | select(.dn=="dn1:8080") | .missing_from_dn | length')
echo "    dn1 still missing: ${DN1_MISS}"
[ "$DN1_MISS" -ge 1 ] || { echo "FAIL: expected dn1 still has missing"; exit 1; }
echo

echo "==> stage 5: anti-entropy/repair without cap → finishes the rest"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair")
echo "$REP" | jq -c '{
  repaired: [(.repairs // [])[] | select(.ok)] | length
}'
REPAIRED2=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.ok)] | length')
[ "$REPAIRED2" -ge 3 ] || { echo "FAIL: expected >= 3 in second pass, got ${REPAIRED2}"; exit 1; }
echo "    ✓ remainder repaired"
echo

echo "==> stage 6: anti-entropy run — all root_match=true"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/run")
ALL_OK=$(echo "$REP" | jq '[.dns[].root_match] | all')
[ "$ALL_OK" = "true" ] || { echo "FAIL: cluster still divergent"; echo "$REP" | jq '.dns'; exit 1; }
echo "    ✓ all root_match=true"
echo

echo "=== PASS: ADR-059 max_repairs caps repair byte movement per call ==="
echo "    Cleanup: ./scripts/down.sh"
