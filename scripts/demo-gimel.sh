#!/usr/bin/env bash
# demo-gimel.sh — Season 5 Ep.3: coord HA via Raft (ADR-038).
#
# 3 coord cluster + 1 edge proxy + 3 DN. Election picks a leader; edge.
# CoordClient transparently follows X-COORD-LEADER on 503. Kill leader →
# new election. Pre-failover writes are LOST in this ep (Ep.4 = ADR-039
# adds WAL replication that closes the gap).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

echo "=== ג gimel demo: coord HA via Raft (Season 5 Ep.3, ADR-038) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true

ensure_images

docker network create $NET 2>/dev/null || true
start_dns 3
for i in 1 2 3; do
  start_coord "coord${i}" $((9000 + i - 1)) "$PEERS_INTERNAL" "http://coord${i}:9000"
done

echo "==> waiting for election (~4s)"
sleep 4

LEADER1=$(find_coord_leader 3 || true)
[ -n "$LEADER1" ] || { echo "FAIL: no leader elected"; for i in 1 2 3; do docker logs "coord${i}" 2>&1 | tail -10; done; exit 1; }
echo "    leader = $LEADER1"

start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"
echo

# PUT (edge follows leader-redirect transparently if coord1 isn't leader).
put_url="${EDGE}$(sign_url PUT /v1/o/gimel/v1 60)"
echo "==> PUT /v1/o/gimel/v1 — edge follows X-COORD-LEADER if needed"
curl -fs -X PUT --data-binary 'gimel write' "$put_url" \
  | jq -c '{bucket, key, size}'

get_url="${EDGE}$(sign_url GET /v1/o/gimel/v1 60)"
GOT=$(curl -fs "$get_url")
[ "$GOT" = "gimel write" ] || { echo "FAIL: GET body=$GOT"; exit 1; }
echo "    GET round-trip ✓"
echo

# Failover.
echo "==> killing leader $LEADER1 → expect re-election"
LEADER_NAME=$(echo "$LEADER1" | cut -d: -f1)
docker kill "$LEADER_NAME" >/dev/null
sleep 5

LEADER2=$(find_coord_leader 3 || true)
[ -n "$LEADER2" ] || { echo "FAIL: no new leader"; exit 1; }
[ "$LEADER1" != "$LEADER2" ] || { echo "FAIL: leader unchanged"; exit 1; }
echo "    new leader = $LEADER2"
echo

put_url=${EDGE}$(sign_url PUT /v1/o/gimel/v2 60)
echo "==> PUT after failover"
curl -fs -X PUT --data-binary 'after failover' "$put_url" \
  | jq -c '{bucket, key, size}'
echo

# Known limitation.
echo "==> known limitation (ADR-038 → ADR-039 fixes): pre-failover writes"
echo "    live on the dead leader's bbolt only. coord-to-coord WAL sync"
echo "    arrives in demo-dalet (Ep.4)."
get_url="${EDGE}$(sign_url GET /v1/o/gimel/v1 60)"
GAP=$(curl -s -o /dev/null -w '%{http_code}' "$get_url")
echo "    GET gimel/v1 → ${GAP} (404 expected for now)"
echo

echo "=== ג PASS: election + leader-redirect protocol works ==="
echo "    Cleanup: ./scripts/down.sh"
