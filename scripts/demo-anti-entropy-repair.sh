#!/usr/bin/env bash
# demo-anti-entropy-repair.sh — P8-08 (ADR-055): anti-entropy auto-repair.
#
# Sequence:
#   1. 3-DN cluster + 1 coord (DN_IO=1) + edge.
#   2. PUT 3 replicated objects → 3 chunks per DN.
#   3. Run anti-entropy: clean → all root_match.
#   4. rm a chunk file from dn1 (simulate disk loss).
#   5. Run /anti-entropy/run → reports dn1 missing=1.
#   6. Run /anti-entropy/repair → reads from dn2 or dn3, PUTs to dn1.
#   7. Re-run /anti-entropy/run → all root_match again.
#   8. (sanity) GET the previously-victim chunk via edge → still works.
#
# Demonstrates that detection (Ep.4) + auto-repair (this ep) closes the
# loop: cluster heals itself without operator intervention beyond the
# trigger.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy auto-repair demo (P8-08, ADR-055) ==="
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

echo "==> stage 1: PUT 3 objects"
for i in 1 2 3; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/repair/obj${i}" 60)"
  curl -fs -X PUT --data-binary "body-${i}-payload-distinguishable" "$put_url" >/dev/null
done
echo "    seeded 3 objects"
echo

echo "==> stage 2: clean anti-entropy run"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/run")
echo "$REP" | jq -c '[.dns[] | {dn, root_match}]'
ALL_OK=$(echo "$REP" | jq '[.dns[].root_match] | all')
[ "$ALL_OK" = "true" ] || { echo "FAIL: expected clean baseline"; exit 1; }
echo

echo "==> stage 3: simulate disk loss — rm one chunk file from dn1"
VICTIM_ID=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" | jq -r '.[0].chunks[0].chunk_id')
VICTIM_PATH="/var/lib/kvfs-dn/chunks/${VICTIM_ID:0:2}/${VICTIM_ID:2}"
docker exec dn1 rm -f "$VICTIM_PATH"
echo "    deleted ${VICTIM_ID:0:16}.. from dn1"
echo

echo "==> stage 4: anti-entropy detects"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/run")
echo "$REP" | jq -c '[.dns[] | {dn, missing: (.missing_from_dn | length)}]'
DN1_MISS=$(echo "$REP" | jq -r '.dns[] | select(.dn=="dn1:8080") | .missing_from_dn | length')
[ "$DN1_MISS" = "1" ] || { echo "FAIL: expected dn1 missing=1, got ${DN1_MISS}"; exit 1; }
echo "    ✓ dn1 missing=1 confirmed"
echo

echo "==> stage 5: auto-repair"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair")
echo "$REP" | jq -c '{
  duration,
  repaired_ok: [.repairs[] | select(.ok)] | length,
  repaired_fail: [.repairs[] | select(.ok | not)] | length,
  skipped: (.skipped // []) | length,
  example: (.repairs[0] // null)
}'
REPAIRED=$(echo "$REP" | jq -r '[.repairs[] | select(.ok)] | length')
[ "$REPAIRED" = "1" ] || { echo "FAIL: expected 1 successful repair, got ${REPAIRED}"; exit 1; }
echo "    ✓ 1 chunk copied from a healthy replica → dn1"
echo

echo "==> stage 6: anti-entropy verifies cluster is clean again"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/run")
echo "$REP" | jq -c '[.dns[] | {dn, root_match}]'
ALL_OK=$(echo "$REP" | jq '[.dns[].root_match] | all')
[ "$ALL_OK" = "true" ] || { echo "FAIL: cluster still divergent post-repair"; exit 1; }
echo "    ✓ all root_match=true again"
echo

echo "==> stage 7: GET the victim object via edge (sanity)"
get_url="${EDGE}$(sign_url GET /v1/o/repair/obj1 60)"
GOT=$(curl -fs "$get_url")
[ "$GOT" = "body-1-payload-distinguishable" ] || { echo "FAIL: GET body=${GOT}"; exit 1; }
echo "    ✓ GET returns intact body"
echo

echo "=== PASS: detection (ADR-054) + auto-repair (ADR-055) close the loop ==="
echo "    Cleanup: ./scripts/down.sh"
