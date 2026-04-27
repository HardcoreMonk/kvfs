#!/usr/bin/env bash
# demo-anti-entropy-repair-ec-corrupt.sh — P8-11 (ADR-058): EC corrupt
# repair. Closes the last self-heal gap from ADR-055.
#
# ADR-057 handled EC missing (file gone). This demo: EC corrupt (file
# present, bytes wrong). The repair worker now uses PutChunkToForce for
# corrupt-flagged shards (DeadShard.Force=true), routed through the same
# repair.Run that handles missing — single worker, two repair scenarios.
#
# Stages:
#   1. 6-DN cluster + coord (DN_IO=1) + edge with scrubber on (50ms).
#   2. PUT 1 MB EC (4+2) object.
#   3. Pick a shard, overwrite its file with garbage on its DN.
#   4. Wait for scrubber to flag it.
#   5. anti-entropy repair?ec=1 (NO corrupt=1) → no work (file present
#      so audit doesn't mark it missing).
#   6. anti-entropy repair?ec=1&corrupt=1 → reconstruct + force overwrite.
#   7. Wait for next scrub pass; corrupt set empty.
#   8. GET via edge → 1MB sha256 intact.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy EC corrupt repair demo (P8-11, ADR-058) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

for i in 1 2 3 4 5 6; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network "$NET" \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -e DN_SCRUB_INTERVAL="50ms" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD_URL}/v1/coord/healthz"

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> stage 1: PUT 1 MB EC (4+2) object"
head -c $((1024 * 1024)) /dev/urandom > /tmp/ec-corrupt-source.bin
EXPECTED_SHA=$(sha256sum /tmp/ec-corrupt-source.bin | awk '{print $1}')
put_url="${EDGE}$(sign_url PUT /v1/o/ec-corrupt/data 60)"
curl -fs -X PUT -H "X-KVFS-EC: 4+2" --data-binary @/tmp/ec-corrupt-source.bin "$put_url" >/dev/null
echo "    expected sha256: ${EXPECTED_SHA:0:32}.."
echo

echo "==> stage 2: corrupt one shard file (overwrite with garbage)"
SHARD_ID=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" | jq -r '.[0].stripes[0].shards[0].chunk_id')
SHARD_DN=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" | jq -r '.[0].stripes[0].shards[0].replicas[0]')
SHARD_DN_NAME="${SHARD_DN%:*}"
SHARD_PATH="/var/lib/kvfs-dn/chunks/${SHARD_ID:0:2}/${SHARD_ID:2}"
docker exec "$SHARD_DN_NAME" sh -c "echo 'corrupted-bytes-NOT-real-shard' > '${SHARD_PATH}'"
echo "    overwrote ${SHARD_ID:0:16}.. on ${SHARD_DN}"
echo

echo "==> stage 3: wait for scrubber to flag the corrupt shard"
sleep 4
SCRUB=$(docker exec "$SHARD_DN_NAME" wget -qO - http://localhost:8080/chunks/scrub-status 2>/dev/null)
echo "    scrub-status on ${SHARD_DN}: $(echo "$SCRUB" | jq -c '{corrupt_count, corrupt}')"
DETECTED=$(echo "$SCRUB" | jq -r --arg id "$SHARD_ID" '.corrupt | index($id) | tostring')
[ "$DETECTED" != "null" ] || { echo "FAIL: scrubber didn't flag"; exit 1; }
echo "    ✓ scrubber flagged"
echo

echo "==> stage 4: ec=1 alone (no corrupt=1) → no EC work (audit doesn't see it)"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?ec=1")
ECINLINE=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.mode=="ec-inline")] | length')
echo "    ec-inline outcomes: ${ECINLINE}"
[ "$ECINLINE" = "0" ] || { echo "FAIL: should be 0 (audit doesn't see corrupt)"; exit 1; }
echo "    ✓ ec=1 alone doesn't pick up corrupt — audit only sees missing"
echo

echo "==> stage 5: ec=1 + corrupt=1 → RS Reconstruct + force overwrite"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?ec=1&corrupt=1")
echo "$REP" | jq -c '{
  ec_inline: [(.repairs // [])[] | select(.mode=="ec-inline" and .ok)] | length,
  ec_summary: [(.repairs // [])[] | select(.mode=="ec-summary")] | first
}'
ECOK=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.mode=="ec-inline" and .ok)] | length')
[ "$ECOK" -ge 1 ] || { echo "FAIL: expected >= 1 ec-inline OK, got ${ECOK}"; exit 1; }
echo "    ✓ corrupt EC shard reconstructed + force-overwritten"
echo

echo "==> stage 6: scrubber re-scans → corrupt set empty"
sleep 4
SCRUB=$(docker exec "$SHARD_DN_NAME" wget -qO - http://localhost:8080/chunks/scrub-status 2>/dev/null)
CCOUNT=$(echo "$SCRUB" | jq -r '.corrupt_count')
[ "$CCOUNT" = "0" ] || { echo "FAIL: scrubber still reports corrupt: $CCOUNT"; exit 1; }
echo "    ✓ scrubber confirms bytes healthy"
echo

echo "==> stage 7: GET via edge — sha256 intact"
get_url="${EDGE}$(sign_url GET /v1/o/ec-corrupt/data 60)"
curl -fs "$get_url" -o /tmp/ec-corrupt-got.bin
GOT_SHA=$(sha256sum /tmp/ec-corrupt-got.bin | awk '{print $1}')
[ "$GOT_SHA" = "$EXPECTED_SHA" ] || { echo "FAIL: sha mismatch"; exit 1; }
echo "    ✓ GET returns intact 1MB body"
rm -f /tmp/ec-corrupt-source.bin /tmp/ec-corrupt-got.bin
echo

echo "=== PASS: ADR-058 closes EC corrupt — full self-heal coverage ==="
echo "    Cleanup: ./scripts/down.sh"
