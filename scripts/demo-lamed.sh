#!/usr/bin/env bash
# demo-lamed.sh — Season 6 Ep.5: DN registry mutation on coord (ADR-047).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
COORD="http://localhost:9000"

echo "=== ל lamed demo: DN registry mutation on coord (Season 6 Ep.5, ADR-047) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true
start_dns 4
COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD}/v1/coord/healthz"

echo "==> initial DN list (cli via coord)"
docker run --rm --network "$NET" kvfs-cli:dev \
  dns --coord http://coord1:9000 list

echo
echo "==> add dn4 via coord"
docker run --rm --network "$NET" kvfs-cli:dev \
  dns --coord http://coord1:9000 add dn4:8080
echo

echo "==> set dn4 class=hot"
docker run --rm --network "$NET" kvfs-cli:dev \
  dns --coord http://coord1:9000 class dn4:8080 hot
echo

echo "==> verify class persisted (read directly from coord bbolt via inspect)"
docker run --rm --network "$NET" kvfs-cli:dev \
  inspect --coord http://coord1:9000 | jq '.dns'
echo

echo "==> remove dn4 via coord"
docker run --rm --network "$NET" kvfs-cli:dev \
  dns --coord http://coord1:9000 remove dn4:8080
echo

echo "==> final list (dn4 gone)"
docker run --rm --network "$NET" kvfs-cli:dev \
  dns --coord http://coord1:9000 list
echo

echo "=== ל PASS: cli mutates DN registry directly on coord ==="
echo "    Cleanup: ./scripts/down.sh"
