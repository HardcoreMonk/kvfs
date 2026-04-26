#!/usr/bin/env bash
# demo-aleph.sh — Season 5 Ep.1: kvfs-coord daemon skeleton (ADR-015).
#
# Boots kvfs-coord standalone (no edge integration yet — that's Ep.2~).
# Exercises the four RPCs directly with curl:
#   POST /v1/coord/place   — placement decision
#   POST /v1/coord/commit  — write ObjectMeta
#   GET  /v1/coord/lookup  — read it back
#   POST /v1/coord/delete  — remove it
#
# Verifies that coord owns its bbolt independently of edge, that placement
# is deterministic, and that commit + lookup round-trip cleanly.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
COORD_PORT=9000
COORD_NAME=coord1

echo "=== ℵ aleph demo: kvfs-coord skeleton (Season 5 Ep.1, ADR-015) ==="
echo "    Hebrew letters mark Season 5 (Greek alpha-omega exhausted at S4 close)"
echo

# Cleanup prior runs.
docker rm -f $COORD_NAME 2>/dev/null || true
docker volume rm coord1-data 2>/dev/null || true
for i in 1 2 3; do
  docker rm -f "dn${i}" 2>/dev/null || true
  docker volume rm "dn${i}-data" 2>/dev/null || true
done

# Build images if missing.
docker images --format '{{.Repository}}:{{.Tag}}' | grep -q '^kvfs-coord:dev$' || {
  echo "==> building images (one-time)"
  docker build --target kvfs-coord -t kvfs-coord:dev . >/dev/null
  docker build --target kvfs-dn    -t kvfs-dn:dev    . >/dev/null
}

docker network create $NET 2>/dev/null || true

# Start 3 DN (coord doesn't actually talk to them yet, but DN list is the
# input to placement).
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" \
    --network $NET \
    -e DN_ID="dn${i}" \
    -e DN_ADDR=":8080" \
    -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

# Start coord with the DN list.
docker volume create coord1-data >/dev/null
docker run -d --name $COORD_NAME \
  --network $NET \
  -p "${COORD_PORT}:9000" \
  -e COORD_ADDR=":9000" \
  -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
  -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -v "coord1-data:/var/lib/kvfs-coord" \
  kvfs-coord:dev >/dev/null

echo "==> waiting for coord to come up"
for _ in $(seq 1 30); do
  curl -fs "http://localhost:${COORD_PORT}/v1/coord/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done
curl -fs "http://localhost:${COORD_PORT}/v1/coord/healthz" \
  || { echo "coord did not come up"; docker logs $COORD_NAME; exit 1; }
echo "    healthz OK"
echo

# 1. PLACE: 3 of 3 DN should be selected, deterministic.
echo "==> POST /v1/coord/place {key:abc-chunk, n:3}"
PLACED=$(curl -fs -X POST -H "Content-Type: application/json" \
  -d '{"key":"abc-chunk","n":3}' \
  "http://localhost:${COORD_PORT}/v1/coord/place" | jq -r '.addrs|join(",")')
echo "    placed → $PLACED"
PLACED2=$(curl -fs -X POST -H "Content-Type: application/json" \
  -d '{"key":"abc-chunk","n":3}' \
  "http://localhost:${COORD_PORT}/v1/coord/place" | jq -r '.addrs|join(",")')
[ "$PLACED" = "$PLACED2" ] || { echo "    FAIL: placement non-deterministic"; exit 1; }
echo "    deterministic across two calls ✓"
echo

# 2. COMMIT: store metadata pointing at those addrs.
echo "==> POST /v1/coord/commit"
META=$(jq -nc \
  --arg addrs "$PLACED" \
  '{
    "meta": {
      "bucket": "demo",
      "key": "ep1/hello",
      "size": 11,
      "chunks": [
        { "chunk_id": "abc-chunk", "size": 11, "replicas": ($addrs | split(",")) }
      ]
    }
  }')
COMMIT=$(curl -fs -X POST -H "Content-Type: application/json" \
  -d "$META" "http://localhost:${COORD_PORT}/v1/coord/commit")
echo "    $COMMIT"
echo

# 3. LOOKUP: read it back.
echo "==> GET /v1/coord/lookup?bucket=demo&key=ep1/hello"
LOOKED=$(curl -fs "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=demo&key=ep1/hello")
echo "    $(echo "$LOOKED" | jq -c '{bucket, key, size, chunks: (.chunks|length)}')"
[ "$(echo "$LOOKED" | jq -r '.bucket')" = "demo" ] || { echo "FAIL: lookup body wrong"; exit 1; }
echo

# 4. DELETE → 404 on next lookup.
echo "==> POST /v1/coord/delete"
curl -fs -X POST -H "Content-Type: application/json" \
  -d '{"bucket":"demo","key":"ep1/hello"}' \
  "http://localhost:${COORD_PORT}/v1/coord/delete" | jq -c .
LOOK_404=$(curl -s -o /dev/null -w '%{http_code}' \
  "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=demo&key=ep1/hello")
[ "$LOOK_404" = "404" ] || { echo "FAIL: post-delete status $LOOK_404"; exit 1; }
echo "    post-delete lookup → 404 ✓"
echo

echo "=== ℵ PASS: coord daemon owns placement + meta independently ==="
echo "    Season 5 진입 — Ep.2 부터 edge 가 coord 의 RPC 를 호출하기 시작."
echo
echo "Cleanup: ./scripts/down.sh   # (or docker rm -f $COORD_NAME dn1 dn2 dn3)"
