#!/usr/bin/env bash
# demo-anti-entropy-repair-corrupt.sh — P8-09 (ADR-056): scrubber-detected
# corrupt repair + dry-run preview.
#
# Closes the OTHER detection-action loop ADR-055 left open. ADR-055 fixed
# inventory missing (chunk file gone). This demo: bit-rot (chunk file
# present, bytes wrong, scrubber flagged it), and the repair worker reads
# from a healthy replica + overwrites.
#
# Stages:
#   1. 3-DN cluster with scrubber on (DN_SCRUB_INTERVAL=50ms).
#   2. PUT 3 objects.
#   3. Overwrite a chunk file on dn2 with garbage. Wait for scrubber pass.
#   4. anti-entropy/repair (default: missing-only) → 0 work
#      (chunk file exists, audit doesn't report it as missing).
#   5. anti-entropy/repair?dry_run=1&corrupt=1 → preview: 1 chunk planned.
#   6. anti-entropy/repair?corrupt=1 → real repair: 1 chunk copied.
#   7. Wait for next scrub pass; verify corrupt set is empty.
#   8. GET via edge (sanity).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy corrupt-repair + dry-run demo (P8-09, ADR-056) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

# Start DNs with scrubber on a fast interval.
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network "$NET" \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -e DN_SCRUB_INTERVAL="50ms" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done
COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD_URL}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> stage 1: PUT 3 objects"
for i in 1 2 3; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/corrupt-repair/obj${i}" 60)"
  curl -fs -X PUT --data-binary "real-body-${i}-distinguishable" "$put_url" >/dev/null
done
echo "    seeded 3 objects"
echo

echo "==> stage 2: simulate bit-rot on dn2 — overwrite chunk file with garbage"
ROT_ID=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" | jq -r '.[1].chunks[0].chunk_id')
ROT_PATH="/var/lib/kvfs-dn/chunks/${ROT_ID:0:2}/${ROT_ID:2}"
docker exec dn2 sh -c "echo 'corrupted-bytes-NOT-the-real-payload' > '${ROT_PATH}'"
echo "    overwrote ${ROT_ID:0:16}.. on dn2"
echo "    waiting 4s for scrubber to flag it..."
sleep 4
SCRUB=$(docker exec dn2 wget -qO - http://localhost:8080/chunks/scrub-status 2>/dev/null)
echo "    scrub-status: $(echo "$SCRUB" | jq -c '{corrupt_count, corrupt}')"
CORRUPT_DETECTED=$(echo "$SCRUB" | jq -r --arg id "$ROT_ID" '.corrupt | index($id) | tostring')
[ "$CORRUPT_DETECTED" != "null" ] || { echo "FAIL: scrubber didn't flag corrupt"; exit 1; }
echo "    ✓ scrubber flagged the corrupt chunk"
echo

echo "==> stage 3: default repair (missing-only) — should find no work"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair")
echo "$REP" | jq -c '{repaired_ok: [(.repairs // [])[] | select(.ok)] | length, dry_run}'
ROK=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.ok)] | length')
[ "$ROK" = "0" ] || { echo "FAIL: default repair shouldn't touch corrupt without --corrupt"; exit 1; }
echo "    ✓ default mode ignores corrupt (chunk file exists, audit reports nothing)"
echo

echo "==> stage 4: dry-run + corrupt — preview what WOULD happen"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?corrupt=1&dry_run=1")
echo "$REP" | jq -c '{
  dry_run,
  planned: [(.repairs // [])[] | select(.planned)] | length,
  example: [(.repairs // [])[] | select(.planned)] | first
}'
PLANNED=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.planned)] | length')
[ "$PLANNED" = "1" ] || { echo "FAIL: expected 1 planned, got ${PLANNED}"; exit 1; }
echo "    ✓ dry-run shows 1 corrupt chunk would be repaired"
# Verify dn2 still has the corrupt bytes (dry-run didn't actually copy).
ON_DISK=$(docker exec dn2 cat "$ROT_PATH" 2>/dev/null)
[ "$ON_DISK" = "corrupted-bytes-NOT-the-real-payload" ] || { echo "FAIL: dry-run shouldn't touch disk"; exit 1; }
echo "    ✓ dn2 disk still corrupt — dry-run didn't move bytes"
echo

echo "==> stage 5: real repair with --corrupt — overwrite the bad bytes"
REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?corrupt=1")
echo "$REP" | jq -c '{
  repaired_ok: [(.repairs // [])[] | select(.ok)] | length,
  reasons: [(.repairs // [])[] | select(.ok) | .reason] | unique
}'
ROK=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.ok)] | length')
[ "$ROK" = "1" ] || { echo "FAIL: expected 1 OK repair, got ${ROK}"; exit 1; }
echo "    ✓ corrupt chunk overwritten from a healthy replica"
echo

echo "==> stage 6: wait for scrubber to re-scan + verify corrupt set empty"
sleep 4
SCRUB=$(docker exec dn2 wget -qO - http://localhost:8080/chunks/scrub-status 2>/dev/null)
echo "    scrub-status: $(echo "$SCRUB" | jq -c '{corrupt_count, scrubbed: .chunks_scrubbed_total}')"
CCOUNT=$(echo "$SCRUB" | jq -r '.corrupt_count')
[ "$CCOUNT" = "0" ] || { echo "FAIL: scrubber still reports corrupt: $CCOUNT"; exit 1; }
echo "    ✓ scrubber's next pass confirmed bytes are healthy"
echo

echo "==> stage 7: GET via edge (sanity)"
get_url="${EDGE}$(sign_url GET /v1/o/corrupt-repair/obj2 60)"
GOT=$(curl -fs "$get_url")
[ "$GOT" = "real-body-2-distinguishable" ] || { echo "FAIL: GET body=${GOT}"; exit 1; }
echo "    ✓ GET returns intact body"
echo

echo "=== PASS: ADR-056 closes the corrupt-repair + dry-run loop ==="
echo "    Cleanup: ./scripts/down.sh"
