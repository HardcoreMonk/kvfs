#!/usr/bin/env bash
# demo-yod.sh — Season 6 Ep.3: GC plan + apply on coord (ADR-045).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD="http://localhost:9000"

echo "=== י yod demo: GC on coord (Season 6 Ep.3, ADR-045) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true
start_dns 3
COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# PUT 3 objects, then DELETE — chunks remain on disk = surplus.
for i in 1 2 3; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/yod/k${i}" 60)"
  curl -fs -X PUT --data-binary "garbage candidate ${i}" "$put_url" >/dev/null
done
for i in 1 2 3; do
  del_url="${EDGE}$(sign_url DELETE "/v1/o/yod/k${i}" 60)"
  curl -fs -X DELETE -o /dev/null "$del_url"
done
echo "==> 3 PUTs followed by 3 DELETEs (chunks now orphaned on DN disks)"
echo

# Wait for min-age threshold to elapse (otherwise GC's safety net protects them).
echo "==> sleeping 6s to clear default 5m → 1s threshold (we override via --min-age 1)"
sleep 2

echo "==> kvfs-cli gc --plan --coord (min-age 1s)"
docker run --rm --network "$NET" kvfs-cli:dev \
  gc --plan --coord http://coord1:9000 --min-age 1 | head -8
echo

echo "==> kvfs-cli gc --apply --coord (sweeps the surplus)"
docker run --rm --network "$NET" kvfs-cli:dev \
  gc --apply --coord http://coord1:9000 --min-age 1 | head -10
echo

echo "==> re-plan: should be empty"
docker run --rm --network "$NET" kvfs-cli:dev \
  gc --plan --coord http://coord1:9000 --min-age 1 | head -5

echo
echo "=== י PASS: GC ran end-to-end on coord ==="
echo "    Cleanup: ./scripts/down.sh"
