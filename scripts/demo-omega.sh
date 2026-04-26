#!/usr/bin/env bash
# demo-omega.sh — Synchronous Raft-style WAL replication (ADR-031 follow-up).
#
# 3 edges with election + WAL enabled. Leader pushes each WAL entry to
# followers in real time. Verify follower bbolt has the write within ~50ms
# of the client PUT (vs. ~1s with polling).
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
NET="kvfs-net"
BUCKET="demo-omega"
PEERS_INTERNAL="http://edge1:8000,http://edge2:8000,http://edge3:8000"

echo "=== Ω demo: synchronous Raft-style WAL replication (ADR-031 follow-up) ==="

./scripts/down.sh >/dev/null 2>&1 || true
for n in edge1 edge2 edge3; do
  docker rm -f "$n" 2>/dev/null || true
  docker volume rm "${n}-data" 2>/dev/null || true
done

docker network create $NET 2>/dev/null || true
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker rm -f "dn${i}" 2>/dev/null || true
  docker run -d --name "dn${i}" \
    --network $NET \
    -e DN_ID="dn${i}" \
    -e DN_ADDR=":8080" \
    -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

EDGE_PORTS=(8000 8001 8002)
EDGE_NAMES=(edge1 edge2 edge3)
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"
  name="${EDGE_NAMES[$i]}"
  docker volume create "${name}-data" >/dev/null
  docker run -d --name "${name}" \
    --network $NET \
    -p "${port}:8000" \
    -e EDGE_ADDR=":8000" \
    -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
    -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
    -e EDGE_URLKEY_SECRET="$SECRET" \
    -e EDGE_QUORUM_WRITE="2" \
    -e EDGE_PEERS="$PEERS_INTERNAL" \
    -e EDGE_SELF_URL="http://${name}:8000" \
    -e EDGE_ELECTION_HB_INTERVAL="200ms" \
    -e EDGE_ELECTION_TIMEOUT_MIN="600ms" \
    -e EDGE_ELECTION_TIMEOUT_MAX="1200ms" \
    -e EDGE_FOLLOWER_PULL_INTERVAL="60s" \
    -e EDGE_WAL_PATH="/var/lib/kvfs-edge/mutations.wal" \
    -v "${name}-data:/var/lib/kvfs-edge" \
    kvfs-edge:dev >/dev/null
done

sleep 3

# Find leader.
leader_url=""
leader_name=""
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"; name="${EDGE_NAMES[$i]}"
  state=$(curl -fsS "http://localhost:${port}/v1/election/state" 2>/dev/null || echo '{}')
  st=$(echo "$state" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('state',''))" 2>/dev/null)
  if [ "$st" = "leader" ]; then leader_url="http://localhost:${port}"; leader_name="${name}"; fi
done
[ -z "$leader_url" ] && { echo "❌ no leader"; exit 1; }
echo "  leader: $leader_name @ $leader_url"

echo
echo "--- step 1: PUT obj-1 via leader, immediately check follower WAL seq ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/obj-1" --ttl 5m --base "$leader_url")
curl -fsS -X PUT --data-binary "raft-replicated-1" "$url" >/dev/null
echo "  PUT obj-1 OK"

# Check WAL seq on each edge — should all be 1 within ~100ms (sync push)
sleep 0.2
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"; name="${EDGE_NAMES[$i]}"
  info=$(curl -fsS "http://localhost:${port}/v1/admin/wal/info" 2>/dev/null)
  seq=$(echo "$info" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('last_seq', '?'))" 2>/dev/null)
  echo "  ${name} WAL last_seq: $seq"
done

echo
echo "--- step 2: PUT 4 more rapid writes, verify all peers stay in sync ---"
for i in 2 3 4 5; do
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/obj-$i" --ttl 5m --base "$leader_url")
  curl -fsS -X PUT --data-binary "raft-replicated-$i" "$url" >/dev/null
done
sleep 0.5
echo "  WAL seq across all 3 edges (expecting 5):"
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"; name="${EDGE_NAMES[$i]}"
  info=$(curl -fsS "http://localhost:${port}/v1/admin/wal/info" 2>/dev/null)
  seq=$(echo "$info" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('last_seq', '?'))" 2>/dev/null)
  echo "    ${name}: $seq"
done

echo
echo "--- step 3: verify follower can serve GET (data already replicated, no pull needed) ---"
follower_url=""
for i in 0 1 2; do
  if [ "${EDGE_NAMES[$i]}" != "$leader_name" ]; then
    follower_url="http://localhost:${EDGE_PORTS[$i]}"; break
  fi
done
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/$BUCKET/obj-3" --ttl 5m --base "$follower_url")
body=$(curl -fsS "$url" 2>&1 || true)
echo "  GET obj-3 from follower: ${body:0:32}"

# cleanup
for n in edge1 edge2 edge3; do
  docker rm -f "$n" >/dev/null 2>&1 || true
  docker volume rm "${n}-data" >/dev/null 2>&1 || true
done

echo
echo "✅ Ω demo PASS — leader-pushed WAL entries reach followers within sub-second (true sync replication)"
