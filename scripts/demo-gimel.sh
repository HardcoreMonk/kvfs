#!/usr/bin/env bash
# demo-gimel.sh — Season 5 Ep.3: coord HA via Raft (ADR-038).
#
# 3 coord cluster + 1 edge (proxy) + 3 DN. Verify:
#   1. Election picks one leader within ~2s
#   2. Edge can PUT through any coord URL — followers redirect via 503+X-COORD-LEADER
#   3. Kill leader → re-election → edge resumes (writes go to new leader)
#
# Known limitation (ADR-038, P6-04 addresses): no coord-to-coord WAL
# sync yet, so the new leader's bbolt is empty — GETs to the old leader's
# data return 404 after failover. The election itself is what we test here.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"

PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

echo "=== ג gimel demo: coord HA via Raft (Season 5 Ep.3, ADR-038) ==="
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

# 3 coords with peers
for i in 1 2 3; do
  port=$((9000 + i - 1))
  docker volume create "coord${i}-data" >/dev/null
  docker run -d --name "coord${i}" --network $NET \
    -p "${port}:9000" \
    -e COORD_ADDR=":9000" -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
    -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080" \
    -e COORD_PEERS="$PEERS_INTERNAL" \
    -e COORD_SELF_URL="http://coord${i}:9000" \
    -v "coord${i}-data:/var/lib/kvfs-coord" \
    kvfs-coord:dev >/dev/null
done

echo "==> waiting for election (~2-4s)"
sleep 4

# Find leader by probing each coord — leader returns 200 on commit, follower 503.
find_leader() {
  for i in 1 2 3; do
    port=$((9000 + i - 1))
    body='{"meta":{"bucket":"_probe","key":"_","size":0,"chunks":[{"chunk_id":"x","size":0,"replicas":["dn1:8080"]}]}}'
    code=$(curl -s -o /dev/null -w '%{http_code}' \
      -X POST -H "Content-Type: application/json" -d "$body" \
      "http://localhost:${port}/v1/coord/commit")
    if [ "$code" = "200" ]; then
      echo "coord${i}:${port}"
      return 0
    fi
  done
  return 1
}

LEADER1=$(find_leader || true)
[ -n "$LEADER1" ] || { echo "FAIL: no leader elected after 4s"; for i in 1 2 3; do docker logs "coord${i}" 2>&1 | tail -10; done; exit 1; }
echo "    leader = $LEADER1"
echo

# edge points at coord1 (deliberately, even if coord1 is follower — redirect test)
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

# PUT — even if coord1 is follower, edge follows X-COORD-LEADER redirect.
echo "==> PUT through edge (CoordClient follows redirect transparently if needed)"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket gimel --key v1 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'gimel write' \
  "http://localhost:8000/v1/o/gimel/v1?$SIG" \
  | jq -c '{bucket, key, size}'

# GET back through edge (any coord can serve lookup).
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method GET --bucket gimel --key v1 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
GOT=$(curl -fs "http://localhost:8000/v1/o/gimel/v1?$SIG")
[ "$GOT" = "gimel write" ] || { echo "FAIL: GET body=$GOT"; exit 1; }
echo "    GET round-trip ✓ (read served by ${LEADER1})"
echo

# Failover: kill the current leader, wait for re-election.
echo "==> killing leader $LEADER1 → expect re-election"
LEADER_NAME=$(echo "$LEADER1" | cut -d: -f1)
docker kill "$LEADER_NAME" >/dev/null
sleep 5

LEADER2=$(find_leader || true)
[ -n "$LEADER2" ] || { echo "FAIL: no new leader within 5s"; exit 1; }
[ "$LEADER1" != "$LEADER2" ] || { echo "FAIL: leader unchanged ($LEADER1)"; exit 1; }
echo "    new leader = $LEADER2"
echo

# A new write goes to the new leader (edge re-discovers via redirect).
echo "==> PUT after failover"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket gimel --key v2 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'after failover' \
  "http://localhost:8000/v1/o/gimel/v2?$SIG" \
  | jq -c '{bucket, key, size}'
echo "    write succeeded against new leader ✓"
echo

# Known gap: pre-failover write (gimel/v1) lives on the dead leader's bbolt,
# new leader's bbolt is empty → GET would 404. Document explicitly.
echo "==> known limitation (ADR-038 → P6-04): pre-failover writes are on the"
echo "    dead leader's bbolt. coord-to-coord WAL sync arrives in the next ep."
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method GET --bucket gimel --key v1 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
GAP=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8000/v1/o/gimel/v1?$SIG")
echo "    GET gimel/v1 (pre-failover) → ${GAP} (404 expected for now)"
echo

echo "=== ג PASS: election + leader-redirect protocol works ==="
echo "    Cleanup: ./scripts/down.sh"
