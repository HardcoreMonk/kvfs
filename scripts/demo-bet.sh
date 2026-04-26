#!/usr/bin/env bash
# demo-bet.sh — Season 5 Ep.2: edge → coord client integration.
#
# 1 coord + 1 edge (proxy mode) + 3 DN. Verify metadata writes go through
# coord (coord.bbolt grows, edge.bbolt stays empty) and that PUT/GET/DELETE
# round-trips work transparently.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_PORT=9000

echo "=== ב bet demo: edge → coord client (Season 5 Ep.2, ADR-015) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true

for tgt in kvfs-coord kvfs-edge kvfs-dn kvfs-cli; do
  docker images --format '{{.Repository}}:{{.Tag}}' | grep -q "^${tgt}:dev$" \
    || { echo "==> building ${tgt}"; docker build --target "${tgt}" -t "${tgt}:dev" . >/dev/null; }
done

docker network create $NET 2>/dev/null || true
start_dns 3
start_coord coord1 $COORD_PORT
wait_healthz "http://localhost:${COORD_PORT}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> edge boot log (coord-proxy mode confirmation):"
docker logs edge 2>&1 | grep -E 'coord-proxy|EDGE_COORD_URL' | head -3 | sed 's/^/    /'
echo

# PUT / GET / DELETE round-trip.
put_url="${EDGE}$(sign_url PUT /v1/o/demo/bet-test 60)"
echo "==> PUT /v1/o/demo/bet-test"
curl -fs -X PUT --data-binary 'hello from bet ep' "$put_url" \
  | jq -c '{bucket, key, size, chunks: (.chunks|length)}'

get_url="${EDGE}$(sign_url GET /v1/o/demo/bet-test 60)"
echo "==> GET /v1/o/demo/bet-test"
GOT=$(curl -fs "$get_url")
echo "    body: $GOT"
[ "$GOT" = "hello from bet ep" ] || { echo "FAIL: body mismatch"; exit 1; }

# Verify coord owns the meta.
echo
echo "==> verify coord owns the meta"
curl -fs "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=demo&key=bet-test" \
  | jq -c '{bucket, key, size}'

# Compare bbolt sizes.
echo
echo "==> edge.db vs coord.db (coord should be larger / authoritative):"
EDGE_DB_BYTES=$(docker exec edge   stat -c%s /var/lib/kvfs-edge/edge.db    2>/dev/null || echo "0")
COORD_DB_BYTES=$(docker exec coord1 stat -c%s /var/lib/kvfs-coord/coord.db 2>/dev/null || echo "0")
echo "    edge.db  = ${EDGE_DB_BYTES} bytes"
echo "    coord.db = ${COORD_DB_BYTES} bytes"

echo
del_url="${EDGE}$(sign_url DELETE /v1/o/demo/bet-test 60)"
echo "==> DELETE /v1/o/demo/bet-test"
curl -fs -X DELETE -o /dev/null -w '    status: %{http_code}\n' "$del_url"

LOOK_404=$(curl -s -o /dev/null -w '%{http_code}' \
  "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=demo&key=bet-test")
[ "$LOOK_404" = "404" ] || { echo "FAIL: coord still has it (status $LOOK_404)"; exit 1; }
echo "    coord lookup → 404 ✓"

echo
echo "=== ב PASS: edge proxies all metadata operations to coord ==="
echo "    Cleanup: ./scripts/down.sh"
