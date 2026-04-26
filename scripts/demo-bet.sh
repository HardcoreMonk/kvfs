#!/usr/bin/env bash
# demo-bet.sh — Season 5 Ep.2: edge → coord client integration.
#
# 1 coord + 1 edge (proxy mode, EDGE_COORD_URL set) + 3 DN.
# PUT/GET/DELETE round-trip through edge as usual; verify coord.bbolt
# (not edge.bbolt) is the one that grows.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
COORD_PORT=9000
EDGE_PORT=8000
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"

echo "=== ב bet demo: edge → coord client (Season 5 Ep.2, ADR-015) ==="
echo

# Cleanup.
./scripts/down.sh >/dev/null 2>&1 || true

# Build images if missing.
for tgt in kvfs-coord kvfs-edge kvfs-dn kvfs-cli; do
  docker images --format '{{.Repository}}:{{.Tag}}' | grep -q "^${tgt}:dev$" || {
    echo "==> building ${tgt} (one-time)"
    docker build --target "${tgt}" -t "${tgt}:dev" . >/dev/null
  }
done

docker network create $NET 2>/dev/null || true

# 3 DN
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" \
    --network $NET \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

# coord
docker volume create coord1-data >/dev/null
docker run -d --name coord1 \
  --network $NET \
  -p "${COORD_PORT}:9000" \
  -e COORD_ADDR=":9000" -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
  -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -v "coord1-data:/var/lib/kvfs-coord" \
  kvfs-coord:dev >/dev/null

# wait for coord healthz
echo "==> waiting for coord"
for _ in $(seq 1 30); do
  curl -fs "http://localhost:${COORD_PORT}/v1/coord/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done

# edge in coord-proxy mode
docker volume create edge-data >/dev/null
docker run -d --name edge \
  --network $NET \
  -p "${EDGE_PORT}:8000" \
  -e EDGE_ADDR=":8000" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_COORD_URL="http://coord1:9000" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

echo "==> waiting for edge"
for _ in $(seq 1 30); do
  curl -fs "http://localhost:${EDGE_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done

# Show edge log line confirming proxy mode.
echo "==> edge boot log (coord-proxy mode confirmation):"
docker logs edge 2>&1 | grep -E 'coord-proxy|EDGE_COORD_URL' | head -3 | sed 's/^/    /'
echo

# PUT
echo "==> PUT /v1/o/demo/bet-test"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket demo --key bet-test --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'hello from bet ep' \
  "http://localhost:${EDGE_PORT}/v1/o/demo/bet-test?$SIG" \
  | jq -c '{bucket, key, size, chunks: (.chunks|length)}'

# GET
echo "==> GET /v1/o/demo/bet-test"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method GET --bucket demo --key bet-test --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
GOT=$(curl -fs "http://localhost:${EDGE_PORT}/v1/o/demo/bet-test?$SIG")
echo "    body: $GOT"
[ "$GOT" = "hello from bet ep" ] || { echo "FAIL: body mismatch"; exit 1; }

# Verify coord owns the meta — call coord directly.
echo
echo "==> verify coord owns the meta (lookup directly on coord)"
COORD_LOOKUP=$(curl -fs "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=demo&key=bet-test")
echo "    $(echo "$COORD_LOOKUP" | jq -c '{bucket, key, size}')"

# Verify edge.bbolt is empty (proxy mode wrote nothing locally).
echo
echo "==> verify edge.bbolt is NOT the source (size of edge's local store)"
EDGE_DB_BYTES=$(docker exec edge stat -c%s /var/lib/kvfs-edge/edge.db 2>/dev/null || echo "0")
COORD_DB_BYTES=$(docker exec coord1 stat -c%s /var/lib/kvfs-coord/coord.db 2>/dev/null || echo "0")
echo "    edge.db  = ${EDGE_DB_BYTES} bytes"
echo "    coord.db = ${COORD_DB_BYTES} bytes"
echo "    (coord.db should be the larger / authoritative one)"

# DELETE
echo
echo "==> DELETE /v1/o/demo/bet-test"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method DELETE --bucket demo --key bet-test --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X DELETE -o /dev/null -w '    status: %{http_code}\n' \
  "http://localhost:${EDGE_PORT}/v1/o/demo/bet-test?$SIG"

# Verify gone via coord.
LOOK_404=$(curl -s -o /dev/null -w '%{http_code}' \
  "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=demo&key=bet-test")
[ "$LOOK_404" = "404" ] || { echo "FAIL: coord still has it after delete (status $LOOK_404)"; exit 1; }
echo "    coord lookup → 404 ✓"
echo

echo "=== ב PASS: edge proxies all metadata operations to coord ==="
echo "    Cleanup: ./scripts/down.sh"
