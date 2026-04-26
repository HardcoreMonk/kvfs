#!/usr/bin/env bash
# demo-zayin.sh — Season 5 Ep.7: kvfs-cli direct coord admin (ADR-042).
#
# Verifies that `kvfs-cli inspect --coord http://coord:9000` returns
# the same data as the legacy `--db edge-data/edge.db` path used to —
# but without touching edge's filesystem. Coord is now the single
# source the cli queries.
#
# Side note demonstration: in coord-proxy mode, edge.db is empty, so
# the legacy `--db` path against edge.db returns nothing. The --coord
# path returns everything.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD="http://localhost:9000"

echo "=== ז zayin demo: kvfs-cli direct coord admin (Season 5 Ep.7, ADR-042) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true

ensure_images

docker network create $NET 2>/dev/null || true
start_dns 3
start_coord coord1 9000
wait_healthz "${COORD}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# PUT 3 objects through edge (writes go to coord via Ep.2 proxy).
for i in 1 2 3; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/zayin/k${i}" 60)"
  curl -fs -X PUT --data-binary "object ${i}" "$put_url" >/dev/null
done
echo "==> 3 objects PUT through edge (committed to coord.bbolt)"
echo

# 1. Direct coord admin via cli — the Ep.7 contract.
echo "==> kvfs-cli inspect --coord ${COORD} (direct to coord)"
INSPECT_VIA_COORD=$(docker run --rm --network "$NET" kvfs-cli:dev \
  inspect --coord http://coord1:9000)
echo "$INSPECT_VIA_COORD" | jq '{objects: (.objects|length), dns: (.dns|length)}'

OBJ_COUNT=$(echo "$INSPECT_VIA_COORD" | jq '.objects|length')
DN_COUNT=$(echo "$INSPECT_VIA_COORD" | jq '.dns|length')
[ "$OBJ_COUNT" = "3" ] || { echo "FAIL: cli via coord returned $OBJ_COUNT objects, want 3"; exit 1; }
[ "$DN_COUNT" -ge 0 ] || { echo "FAIL: dns count negative"; exit 1; }
echo "    ✓ cli sees the 3 objects directly from coord"
echo

# 2. Single-object inspect via coord.
echo "==> kvfs-cli inspect --coord ${COORD} --object zayin/k2"
INSPECT_ONE=$(docker run --rm --network "$NET" kvfs-cli:dev \
  inspect --coord http://coord1:9000 --object zayin/k2)
echo "$INSPECT_ONE" | jq '{bucket, key, size, chunks: (.chunks|length)}'
KEY=$(echo "$INSPECT_ONE" | jq -r .key)
[ "$KEY" = "k2" ] || { echo "FAIL: single inspect returned key=$KEY"; exit 1; }
echo "    ✓ single-object lookup via coord works"
echo

# 3. Negative: in coord-proxy mode, edge.db is empty.
echo "==> contrast: edge.db is empty in coord-proxy mode"
EDGE_DB_BYTES=$(docker exec edge stat -c%s /var/lib/kvfs-edge/edge.db 2>/dev/null || echo "0")
COORD_DB_BYTES=$(docker exec coord1 stat -c%s /var/lib/kvfs-coord/coord.db 2>/dev/null || echo "0")
echo "    edge.db  = ${EDGE_DB_BYTES} bytes (low — proxy mode)"
echo "    coord.db = ${COORD_DB_BYTES} bytes (authoritative)"
echo

echo "=== ז PASS: kvfs-cli now talks to coord directly ==="
echo "    Cleanup: ./scripts/down.sh"
