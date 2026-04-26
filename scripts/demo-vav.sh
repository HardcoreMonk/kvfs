#!/usr/bin/env bash
# demo-vav.sh — Season 5 Ep.6: edge routes placement through coord (ADR-041).
#
# The trick: coord knows only dn1,dn2,dn3 — but edge knows ALL FOUR
# (dn1..dn4). If edge did its own placement, dn4 would land in some
# chunk's replica set (HRW with N=4). Since edge routes placement
# through coord (which only knows 3 DNs), dn4 should NEVER appear.
#
# Verifies: object PUT → coord.lookup → chunk replicas ⊆ {dn1,dn2,dn3}.
# Negative: with EDGE_COORD_URL unset (inline mode), dn4 may appear.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_PORT=9000

echo "=== ו vav demo: edge → coord placement (Season 5 Ep.6, ADR-041) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true

ensure_images

docker network create $NET 2>/dev/null || true
start_dns 4   # 4 DNs spun up

# Coord uses default 3-DN set (dn1..dn3). NOT dn4.
start_coord coord1 $COORD_PORT
wait_healthz "http://localhost:${COORD_PORT}/v1/coord/healthz"

# Edge knows ALL FOUR DNs (so dn4 is reachable for I/O), but proxies
# placement to coord (which won't pick dn4) — EDGE_DNS env override.
EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> coord knows: dn1,dn2,dn3   edge knows: dn1,dn2,dn3,dn4"
echo "    placement-via-coord => chunks must NEVER land on dn4"
echo

# PUT 5 objects (different keys → different placement).
for i in 1 2 3 4 5; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/vav/k${i}" 60)"
  curl -fs -X PUT --data-binary "object ${i}" "$put_url" >/dev/null
done
echo "==> 5 objects PUT through edge (placement RPC'd to coord each chunk)"
echo

# Inspect coord's view of every object's chunk replicas.
echo "==> chunk replicas as recorded in coord.bbolt:"
DN4_HITS=0
for i in 1 2 3 4 5; do
  meta=$(curl -fs "http://localhost:${COORD_PORT}/v1/coord/lookup?bucket=vav&key=k${i}")
  replicas=$(echo "$meta" | jq -r '.chunks[0].replicas | join(",")')
  echo "    vav/k${i} -> ${replicas}"
  if echo "$replicas" | grep -q "dn4:8080"; then
    DN4_HITS=$((DN4_HITS + 1))
  fi
done
echo

if [ "$DN4_HITS" -gt 0 ]; then
  echo "    FAIL: dn4 appeared in $DN4_HITS object(s) — coord placement was bypassed"
  exit 1
fi
echo "    ✓ dn4 (which only edge knows) never picked → coord owns placement"
echo

# Quick sanity: GET works (chunks landed on actually-running DNs).
get_url="${EDGE}$(sign_url GET /v1/o/vav/k1 60)"
GOT=$(curl -fs "$get_url")
[ "$GOT" = "object 1" ] || { echo "FAIL: GET body=$GOT"; exit 1; }
echo "==> GET vav/k1 round-trip ✓"

echo
echo "=== ו PASS: edge placement decisions are now coord's responsibility ==="
echo "    Cleanup: ./scripts/down.sh"
