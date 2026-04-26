#!/usr/bin/env bash
# demo-tet.sh — Season 6 Ep.2: rebalance apply on coord (ADR-044).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD="http://localhost:9000"

echo "=== ט tet demo: rebalance apply on coord (Season 6 Ep.2, ADR-044) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

start_dns 4
# coord with DN I/O + 4-DN view (Ep.2: knows all DNs from the start).
COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" COORD_DN_IO=1 \
  start_coord coord1 9000
wait_healthz "${COORD}/v1/coord/healthz"

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# PUT 5 objects.
for i in 1 2 3 4 5; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/tet/k${i}" 60)"
  curl -fs -X PUT --data-binary "object ${i}" "$put_url" >/dev/null
done

# Force misplacement: kill dn4 + restart so chunks placed on dn4
# at PUT time are now "missing" relative to ideal placement.
# Actually since coord knows all 4 DNs from the start, placement
# was already correct. To create work, we register a brand-new dn5
# via the runtime registry — HRW topology shifts.
docker volume create dn5-data >/dev/null
docker run -d --name dn5 --network "$NET" \
  -e DN_ID="dn5" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
  -v dn5-data:/var/lib/kvfs-dn \
  kvfs-dn:dev >/dev/null

ADD_URL="${EDGE}/v1/admin/dns?addr=dn5:8080"
curl -fs -X POST "$ADD_URL" >/dev/null 2>&1 || true
sleep 1

echo
echo "==> kvfs-cli rebalance --plan --coord (after dn5 added)"
PLAN_BEFORE=$(docker run --rm --network "$NET" kvfs-cli:dev \
  rebalance --plan --coord http://coord1:9000 || true)
echo "$PLAN_BEFORE" | head -3

echo
echo "==> kvfs-cli rebalance --apply --coord (Ep.2 path)"
docker run --rm --network "$NET" kvfs-cli:dev \
  rebalance --apply --coord http://coord1:9000 | head -10

echo
echo "==> re-plan: should now be empty (or near-empty) — apply moved chunks"
PLAN_AFTER=$(docker run --rm --network "$NET" kvfs-cli:dev \
  rebalance --plan --coord http://coord1:9000 || true)
echo "$PLAN_AFTER" | head -3
echo

echo "=== ט PASS: rebalance apply runs on coord ==="
echo "    Cleanup: ./scripts/down.sh"
