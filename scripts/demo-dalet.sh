#!/usr/bin/env bash
# demo-dalet.sh — Season 5 Ep.4: coord-to-coord WAL replication (ADR-039).
#
# demo-gimel + COORD_WAL_PATH on every coord. Pre-failover writes survive
# leader kill (the gimel 404 → dalet 200).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

echo "=== ד dalet demo: coord WAL replication (Season 5 Ep.4, ADR-039) ==="
echo "    Closes the demo-gimel data-sync gap."

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true

for tgt in kvfs-coord kvfs-edge kvfs-dn kvfs-cli; do
  docker images --format '{{.Repository}}:{{.Tag}}' | grep -q "^${tgt}:dev$" \
    || { echo "==> building ${tgt}"; docker build --target "${tgt}" -t "${tgt}:dev" . >/dev/null; }
done

docker network create $NET 2>/dev/null || true
start_dns 3
for i in 1 2 3; do
  start_coord "coord${i}" $((9000 + i - 1)) \
    "$PEERS_INTERNAL" "http://coord${i}:9000" \
    "/var/lib/kvfs-coord/coord.wal"
done

echo "==> waiting for election + WAL wiring (~4s)"
sleep 4

LEADER1=$(find_coord_leader 3 || true)
[ -n "$LEADER1" ] || { echo "FAIL: no leader"; exit 1; }
echo "    leader = $LEADER1"

start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"
echo

# Pre-failover write.
put_url="${EDGE}$(sign_url PUT /v1/o/dalet/v1 60)"
echo "==> PUT dalet/v1 (pre-failover)"
curl -fs -X PUT --data-binary 'before failover, with WAL push' "$put_url" \
  | jq -c '{bucket, key, size}'

# Let leader push to followers.
sleep 1

echo
echo "==> follower coord bbolts after pre-failover write:"
for i in 1 2 3; do
  size=$(docker exec   "coord${i}" stat -c%s /var/lib/kvfs-coord/coord.db  2>/dev/null || echo "?")
  walsz=$(docker exec  "coord${i}" stat -c%s /var/lib/kvfs-coord/coord.wal 2>/dev/null || echo "?")
  echo "    coord${i}: bbolt=${size}B wal=${walsz}B"
done
echo

# Failover.
echo "==> killing leader $LEADER1 → expect data still recoverable"
LEADER_NAME=$(echo "$LEADER1" | cut -d: -f1)
docker kill "$LEADER_NAME" >/dev/null
sleep 5

LEADER2=$(find_coord_leader 3 || true)
[ -n "$LEADER2" ] || { echo "FAIL: no new leader"; exit 1; }
[ "$LEADER1" != "$LEADER2" ] || { echo "FAIL: leader unchanged"; exit 1; }
echo "    new leader = $LEADER2"
echo

# Key assertion: pre-failover write recoverable from new leader.
get_url="${EDGE}$(sign_url GET /v1/o/dalet/v1 60)"
echo "==> GET dalet/v1 from new leader (was 404 in demo-gimel; expect 200)"
GAP_FILLED=$(curl -s -o /dev/null -w '%{http_code}' "$get_url")
[ "$GAP_FILLED" = "200" ] || { echo "FAIL: GET returned $GAP_FILLED, expected 200"; exit 1; }
GOT=$(curl -fs "$get_url")
echo "    body recovered: $GOT"
[ "$GOT" = "before failover, with WAL push" ] || { echo "FAIL: body mismatch"; exit 1; }
echo "    ✓ ADR-039 closed the gap."
echo

# Post-failover write.
put_url="${EDGE}$(sign_url PUT /v1/o/dalet/v2 60)"
echo "==> PUT dalet/v2 (post-failover)"
curl -fs -X PUT --data-binary 'post failover write' "$put_url" \
  | jq -c '{bucket, key, size}'

echo
echo "=== ד PASS: coord WAL replication = HA without data loss ==="
echo "    Cleanup: ./scripts/down.sh"
