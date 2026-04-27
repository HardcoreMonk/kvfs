#!/usr/bin/env bash
# demo-anti-entropy-repair-ec.sh — P8-10 (ADR-057): EC inline repair via
# anti-entropy. Closes the last gap from ADR-055 — EC stripes were
# reported as "ec-deferred" and required a separate ADR-046 worker run.
# Now the same /anti-entropy/repair endpoint handles both replication
# and EC chunks with a single ?ec=1 flag.
#
# Stages:
#   1. 6-DN cluster + coord (DN_IO=1) + edge.
#   2. PUT a 1 MB EC (4+2) object → 6 shards across DN.
#   3. Bring up dn5/dn6 deletion: rm one shard file from one of them.
#   4. Anti-entropy detects the missing shard.
#   5. Default repair (no ec=1) → reports as ec-deferred (back-compat).
#   6. anti-entropy repair?ec=1 → reconstruct + write back.
#   7. Anti-entropy clean again.
#   8. GET via edge → intact 1MB body.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy EC inline repair demo (P8-10, ADR-057) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

start_dns 6
COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD_URL}/v1/coord/healthz"

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> stage 1: PUT 1 MB object with EC (4+2)"
head -c $((1024 * 1024)) /dev/urandom > /tmp/ec-source.bin
EXPECTED_SHA=$(sha256sum /tmp/ec-source.bin | awk '{print $1}')
put_url="${EDGE}$(sign_url PUT /v1/o/ec-repair/data 60)"
curl -fs -X PUT -H "X-KVFS-EC: 4+2" --data-binary @/tmp/ec-source.bin "$put_url" >/dev/null
echo "    expected sha256: ${EXPECTED_SHA:0:32}.."
echo

echo "==> stage 2: pick a shard from stripe 0, rm its file from its DN"
SHARD0_ID=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" | jq -r '.[0].stripes[0].shards[0].chunk_id')
SHARD0_DN=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" | jq -r '.[0].stripes[0].shards[0].replicas[0]')
SHARD0_DN_NAME="${SHARD0_DN%:*}"   # dn1:8080 -> dn1
SHARD0_PATH="/var/lib/kvfs-dn/chunks/${SHARD0_ID:0:2}/${SHARD0_ID:2}"
docker exec "$SHARD0_DN_NAME" rm -f "$SHARD0_PATH"
echo "    deleted ${SHARD0_ID:0:16}.. from ${SHARD0_DN}"
echo

echo "==> stage 3: anti-entropy detects (default repair = ec-deferred)"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair")
echo "$REP" | jq -c '{
  repaired: [(.repairs // [])[] | select(.ok)] | length,
  skipped_ec: [(.skipped // [])[] | select(.mode=="ec-deferred")] | length
}'
SKIP=$(echo "$REP" | jq -r '[(.skipped // [])[] | select(.mode=="ec-deferred")] | length')
[ "$SKIP" = "1" ] || { echo "FAIL: expected 1 ec-deferred, got ${SKIP}"; exit 1; }
echo "    ✓ default mode reports ec-deferred (back-compat with ADR-055)"
echo

echo "==> stage 4: anti-entropy repair?ec=1 — RS Reconstruct + write back"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?ec=1")
echo "$REP" | jq -c '{
  ec_inline: [(.repairs // [])[] | select(.mode=="ec-inline")] | length,
  ec_summary: [(.repairs // [])[] | select(.mode=="ec-summary")] | first
}'
ECINLINE=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.mode=="ec-inline" and .ok)] | length')
[ "$ECINLINE" -ge 1 ] || { echo "FAIL: expected >= 1 ec-inline OK, got ${ECINLINE}"; exit 1; }
echo "    ✓ EC stripe reconstructed from K survivors + missing shard written back"
echo

echo "==> stage 5: anti-entropy verifies cluster clean again"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/run")
ALL_OK=$(echo "$REP" | jq '[.dns[].root_match] | all')
[ "$ALL_OK" = "true" ] || { echo "FAIL: divergent post-repair"; echo "$REP" | jq '.dns'; exit 1; }
echo "    ✓ all DN root_match=true again"
echo

echo "==> stage 6: GET via edge — sha256 intact"
get_url="${EDGE}$(sign_url GET /v1/o/ec-repair/data 60)"
curl -fs "$get_url" -o /tmp/ec-got.bin
GOT_SHA=$(sha256sum /tmp/ec-got.bin | awk '{print $1}')
[ "$GOT_SHA" = "$EXPECTED_SHA" ] || { echo "FAIL: sha mismatch"; exit 1; }
echo "    ✓ GET returns intact 1MB body"
rm -f /tmp/ec-source.bin /tmp/ec-got.bin
echo

echo "=== PASS: ADR-057 unifies replication + EC repair under anti-entropy ==="
echo "    Cleanup: ./scripts/down.sh"
