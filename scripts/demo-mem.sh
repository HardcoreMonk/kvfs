#!/usr/bin/env bash
# demo-mem.sh — Season 6 Ep.6: URLKey kid registry on coord (ADR-048).
#
# Demonstrates the registry-side rotation: cli urlkey rotate/list/remove
# all hit coord directly. Note: edge's IN-MEMORY Signer is NOT updated
# by these calls — that propagation is a separate ep (see ADR-048
# "비포함" section).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
COORD="http://localhost:9000"

echo "=== מ mem demo: URLKey on coord (Season 6 Ep.6, ADR-048) ==="

need curl; need jq; need docker
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true
start_dns 1
start_coord coord1 9000
wait_healthz "${COORD}/v1/coord/healthz"

echo "==> initial urlkey list (empty bbolt)"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 list
echo

echo "==> rotate kid=v1 (auto-gen secret, primary)"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 rotate --kid v1
echo

echo "==> list — should show v1 as *primary"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 list
echo

echo "==> rotate kid=v2 (new primary, v1 demoted)"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 rotate --kid v2
echo

echo "==> list — v2 should now be primary, v1 still present"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 list
echo

echo "==> remove v1 (was demoted; allowed)"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 remove --kid v1
echo

echo "==> attempt to remove v2 (current primary; should fail)"
if docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 remove --kid v2 2>&1 | head -3; then
  echo "  (expected error from store: cannot delete primary kid)"
fi
echo

echo "=== מ PASS: URLKey kid registry mutates via coord ==="
echo "    Note: edge in-memory Signer doesn't see these changes yet — separate ADR."
echo "    Cleanup: ./scripts/down.sh"
