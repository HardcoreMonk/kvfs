#!/usr/bin/env bash
# demo-kaf.sh — Season 6 Ep.4: EC repair on coord (ADR-046).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD="http://localhost:9000"

echo "=== כ kaf demo: EC repair on coord (Season 6 Ep.4, ADR-046) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

# 6 DN for EC (4+2)
start_dns 6
docker volume create coord1-data >/dev/null
docker run -d --name coord1 --network "$NET" \
  -p "9000:9000" \
  -e COORD_ADDR=":9000" -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
  -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  -e COORD_DN_IO=1 \
  -v coord1-data:/var/lib/kvfs-coord \
  kvfs-coord:dev >/dev/null
wait_healthz "${COORD}/v1/coord/healthz"

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# EC PUT (4+2): one object → 1 stripe → 6 shards across 6 DNs.
put_url="${EDGE}$(sign_url PUT /v1/o/kaf/v1 60)"
curl -fs -X PUT \
  -H "X-KVFS-EC: 4+2" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @<(head -c 65536 /dev/urandom) \
  "$put_url" | jq -c '{bucket, key, stripes}'
echo "==> EC PUT done — 1 stripe × 6 shards distributed"
echo

# Kill 2 DNs to lose 2 shards (still K=4 survivors, recoverable).
echo "==> killing dn5 + dn6 (lose 2 shards; K=4 survivors remain)"
docker kill dn5 dn6 >/dev/null
sleep 2

echo
echo "==> kvfs-cli repair --plan --coord (should detect missing shards)"
docker run --rm --network "$NET" kvfs-cli:dev \
  repair --plan --coord http://coord1:9000 | head -10
echo

# Bring DNs back as fresh containers (so reconstruct has somewhere to write).
docker rm -f dn5 dn6 >/dev/null
docker volume rm dn5-data dn6-data >/dev/null 2>&1 || true
for i in 5 6; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network $NET \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done
sleep 2

echo "==> kvfs-cli repair --apply --coord (Reed-Solomon reconstruct)"
docker run --rm --network "$NET" kvfs-cli:dev \
  repair --apply --coord http://coord1:9000 | head -10
echo

echo "==> re-plan (should be empty)"
docker run --rm --network "$NET" kvfs-cli:dev \
  repair --plan --coord http://coord1:9000 | head -5
echo

echo "=== כ PASS: EC repair runs end-to-end on coord ==="
echo "    Cleanup: ./scripts/down.sh"
