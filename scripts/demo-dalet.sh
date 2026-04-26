#!/usr/bin/env bash
# demo-dalet.sh — Season 5 Ep.4: coord-to-coord WAL replication (ADR-039).
#
# Same shape as demo-gimel (3 coords + edge proxy) PLUS COORD_WAL_PATH set
# on every coord. Now leader pushes each WAL entry to followers; followers
# ApplyEntry to local bbolt. Verify:
#   1. Pre-failover writes survive a leader kill (the gimel 404 → dalet 200)
#   2. Post-failover writes also work (sanity)
#
# Diff vs gimel: GET to a pre-failover key returns 200 instead of 404.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"

PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

echo "=== ד dalet demo: coord WAL replication (Season 5 Ep.4, ADR-039) ==="
echo "    Closes the demo-gimel data-sync gap."
echo

./scripts/down.sh >/dev/null 2>&1 || true

for tgt in kvfs-coord kvfs-edge kvfs-dn kvfs-cli; do
  docker images --format '{{.Repository}}:{{.Tag}}' | grep -q "^${tgt}:dev$" || {
    echo "==> building ${tgt} (one-time)"
    docker build --target "${tgt}" -t "${tgt}:dev" . >/dev/null
  }
done

docker network create $NET 2>/dev/null || true

# 3 DN
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network $NET \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

# 3 coords with peers + WAL
for i in 1 2 3; do
  port=$((9000 + i - 1))
  docker volume create "coord${i}-data" >/dev/null
  docker run -d --name "coord${i}" --network $NET \
    -p "${port}:9000" \
    -e COORD_ADDR=":9000" -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
    -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080" \
    -e COORD_PEERS="$PEERS_INTERNAL" \
    -e COORD_SELF_URL="http://coord${i}:9000" \
    -e COORD_WAL_PATH="/var/lib/kvfs-coord/coord.wal" \
    -v "coord${i}-data:/var/lib/kvfs-coord" \
    kvfs-coord:dev >/dev/null
done

echo "==> waiting for election + WAL wiring (~4s)"
sleep 4

find_leader() {
  for i in 1 2 3; do
    port=$((9000 + i - 1))
    body='{"meta":{"bucket":"_probe","key":"_","size":0,"chunks":[{"chunk_id":"x","size":0,"replicas":["dn1:8080"]}]}}'
    code=$(curl -s -o /dev/null -w '%{http_code}' \
      -X POST -H "Content-Type: application/json" -d "$body" \
      "http://localhost:${port}/v1/coord/commit")
    if [ "$code" = "200" ]; then echo "coord${i}:${port}"; return 0; fi
  done
  return 1
}

LEADER1=$(find_leader || true)
[ -n "$LEADER1" ] || { echo "FAIL: no leader"; exit 1; }
echo "    leader = $LEADER1"
echo

# edge in proxy mode
docker volume create edge-data >/dev/null
docker run -d --name edge --network $NET -p "8000:8000" \
  -e EDGE_ADDR=":8000" -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_COORD_URL="http://coord1:9000" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

for _ in $(seq 1 30); do
  curl -fs "http://localhost:8000/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done

# Pre-failover write.
echo "==> PUT dalet/v1 (pre-failover)"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket dalet --key v1 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'before failover, with WAL push' \
  "http://localhost:8000/v1/o/dalet/v1?$SIG" \
  | jq -c '{bucket, key, size}'

# Give the leader a moment to push to followers.
sleep 1

# Verify followers also have the entry by inspecting their bbolt size.
echo
echo "==> follower coord bbolts after pre-failover write:"
for i in 1 2 3; do
  size=$(docker exec "coord${i}" stat -c%s /var/lib/kvfs-coord/coord.db 2>/dev/null || echo "?")
  walsz=$(docker exec "coord${i}" stat -c%s /var/lib/kvfs-coord/coord.wal 2>/dev/null || echo "?")
  echo "    coord${i}: bbolt=${size}B wal=${walsz}B"
done
echo "    (all three should have similar sizes — replication landed)"
echo

# Failover: kill the leader.
echo "==> killing leader $LEADER1 → expect re-election + DATA STILL THERE"
LEADER_NAME=$(echo "$LEADER1" | cut -d: -f1)
docker kill "$LEADER_NAME" >/dev/null
sleep 5

LEADER2=$(find_leader || true)
[ -n "$LEADER2" ] || { echo "FAIL: no new leader"; exit 1; }
[ "$LEADER1" != "$LEADER2" ] || { echo "FAIL: leader unchanged"; exit 1; }
echo "    new leader = $LEADER2"
echo

# THE KEY ASSERTION: pre-failover write is recoverable from the new leader.
echo "==> GET dalet/v1 from new leader (was 404 in demo-gimel; expect 200 now)"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method GET --bucket dalet --key v1 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
GAP_FILLED=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8000/v1/o/dalet/v1?$SIG")
if [ "$GAP_FILLED" != "200" ]; then
  echo "    FAIL: GET returned $GAP_FILLED, expected 200 (WAL push didn't reach the new leader before failover)"
  exit 1
fi
GOT=$(curl -fs "http://localhost:8000/v1/o/dalet/v1?$SIG")
echo "    body recovered: $GOT"
[ "$GOT" = "before failover, with WAL push" ] || { echo "FAIL: body mismatch"; exit 1; }
echo "    ✓ ADR-039 closed the gap."
echo

# Post-failover write to demonstrate continued operation.
echo "==> PUT dalet/v2 (post-failover, against the new leader)"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket dalet --key v2 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'post failover write' \
  "http://localhost:8000/v1/o/dalet/v2?$SIG" \
  | jq -c '{bucket, key, size}'
echo

echo "=== ד PASS: coord WAL replication = HA without data loss ==="
echo "    Cleanup: ./scripts/down.sh"
