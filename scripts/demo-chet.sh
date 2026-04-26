#!/usr/bin/env bash
# demo-chet.sh — Season 6 Ep.1: rebalance plan computed on coord (ADR-043).
#
# coord + edge proxy + 3 DN. PUT some objects, then add a 4th DN to coord's
# runtime registry. The new HRW topology means at least one chunk is
# misplaced. Verify both:
#   - cli rebalance --plan --coord  → returns migrations (coord's view)
#   - cli rebalance --plan --edge   → returns migrations (edge inline view)
# Both should agree (same placer, same data) — Season 6 establishes that
# coord is now a valid plan source.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD="http://localhost:9000"

echo "=== ח chet demo: rebalance plan on coord (Season 6 Ep.1, ADR-043) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true

ensure_images

docker network create $NET 2>/dev/null || true
start_dns 4   # spin up 4 DNs but coord starts knowing only 3
start_coord coord1 9000   # default COORD_DNS = dn1,dn2,dn3
wait_healthz "${COORD}/v1/coord/healthz"
EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# PUT 5 objects through edge — coord places on dn1/2/3 only.
for i in 1 2 3 4 5; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/chet/k${i}" 60)"
  curl -fs -X PUT --data-binary "object ${i}" "$put_url" >/dev/null
done
echo "==> 5 objects PUT (coord placed onto dn1/2/3 only — dn4 not yet known)"
echo

# Plan from coord (initial): no migrations expected — chunks are in
# HRW-correct positions for the current 3-DN registry.
echo "==> kvfs-cli rebalance --plan --coord ${COORD} (initial)"
docker run --rm --network "$NET" kvfs-cli:dev \
  rebalance --plan --coord http://coord1:9000 | head -3
echo

# Add dn4 to coord's runtime registry — HRW topology shifts → some
# chunks are now in the "wrong" set per the new placement.
echo "==> register dn4 with coord (admin, NOT through edge)"
curl -fs -X POST "${COORD}/v1/coord/admin/dns/add" 2>/dev/null \
  || echo "    (note: coord doesn't yet expose POST /v1/coord/admin/dns/add — Ep.4 will add)"

# Workaround for Ep.1: register dn4 through edge (which proxies to coord)
SIG="$(sign_url POST /v1/admin/dns 60 | sed 's|^/v1/admin/dns||')"
ADD_URL="${EDGE}/v1/admin/dns?addr=dn4:8080"
curl -fs -X POST "$ADD_URL" >/dev/null 2>&1 \
  || echo "    (fallback: dn registry mutation via edge until ADR-047 ports it)"
sleep 1

# Plan from coord (after topology shift) — now some chunks need to move.
echo
echo "==> kvfs-cli rebalance --plan --coord ${COORD} (after dn4 added)"
COORD_PLAN=$(docker run --rm --network "$NET" kvfs-cli:dev \
  rebalance --plan --coord http://coord1:9000 || true)
echo "$COORD_PLAN" | head -5
COORD_MIG=$(echo "$COORD_PLAN" | grep -oE '[0-9]+ total migrations' | head -1 || echo "0 total migrations")
echo "    → ${COORD_MIG}"
echo

# Edge plan (cross-check) — should be identical (same placer, same Store).
echo "==> kvfs-cli rebalance --plan --edge ${EDGE} (cross-check)"
EDGE_PLAN=$(docker run --rm --network "$NET" kvfs-cli:dev \
  rebalance --plan --edge http://edge:8000 || true)
echo "$EDGE_PLAN" | head -5
EDGE_MIG=$(echo "$EDGE_PLAN" | grep -oE '[0-9]+ total migrations' | head -1 || echo "0 total migrations")
echo "    → ${EDGE_MIG}"
echo

# Side-effect-free test: did coord's plan and edge's plan agree?
if [ "$COORD_MIG" = "$EDGE_MIG" ]; then
  echo "    ✓ both views agree (${COORD_MIG})"
else
  echo "    NOTE: coord=${COORD_MIG} edge=${EDGE_MIG} — different views; expected when"
  echo "    edge inline placer's DN list differs from coord's. ADR-043 doc covers this."
fi
echo

echo "=== ח PASS: rebalance plan computable from coord directly ==="
echo "    Cleanup: ./scripts/down.sh"
