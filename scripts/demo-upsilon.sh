#!/usr/bin/env bash
# demo-upsilon.sh — Auto leader election (Raft-style, ADR-031).
#
# 3 edges sharing same DN cluster. First leader emerges, PUT works.
# Kill the leader, watch the cluster auto-elect a new one within seconds,
# then verify PUT continues working through the new leader.
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
NET="kvfs-net"
BUCKET="demo-upsilon"

# Three edges on different host ports, sharing the same DN cluster.
EDGE_PORTS=(8000 8001 8002)
EDGE_NAMES=(edge1 edge2 edge3)
PEERS_INTERNAL="http://edge1:8000,http://edge2:8000,http://edge3:8000"

echo "=== υ demo: auto leader election (ADR-031) ==="

# 0. clean
./scripts/down.sh >/dev/null 2>&1 || true
for n in edge1 edge2 edge3; do
  docker rm -f "$n" 2>/dev/null || true
  docker volume rm "${n}-data" 2>/dev/null || true
done

# 1. up DN cluster (no edge — we'll start 3 edges manually)
docker network create "$NET" 2>/dev/null || true
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker rm -f "dn${i}" 2>/dev/null || true
  docker run -d --name "dn${i}" \
    --network "$NET" \
    -e DN_ID="dn${i}" \
    -e DN_ADDR=":8080" \
    -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
  echo "  started dn${i}"
done

# 2. up 3 edges with EDGE_PEERS election config
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"
  name="${EDGE_NAMES[$i]}"
  docker volume create "${name}-data" >/dev/null
  docker run -d --name "${name}" \
    --network "$NET" \
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
    -e EDGE_FOLLOWER_PULL_INTERVAL="1s" \
    -v "${name}-data:/var/lib/kvfs-edge" \
    kvfs-edge:dev >/dev/null
  echo "  started ${name} (host port ${port})"
done

# 3. Wait for cluster to be reachable + elect a leader.
sleep 3
echo
echo "--- step 1: which edge is leader? ---"
leader_url=""
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"
  name="${EDGE_NAMES[$i]}"
  state=$(curl -fsS "http://localhost:${port}/v1/election/state" 2>/dev/null || echo '{}')
  s=$(echo "$state" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('state','?'), 'term=', d.get('current_term','?'))" 2>/dev/null || echo "?")
  echo "  ${name} (port ${port}): $s"
  st=$(echo "$state" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('state',''))" 2>/dev/null)
  if [ "$st" = "leader" ]; then
    leader_url="http://localhost:${port}"
    leader_name="${name}"
  fi
done
[ -z "$leader_url" ] && { echo "❌ no leader found"; exit 1; }
echo "  → leader: $leader_name @ $leader_url"

# 4. PUT 1 via leader
echo
echo "--- step 2: PUT obj-1 via leader ($leader_name) ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/obj-1" --ttl 5m --base "$leader_url")
curl -fsS -X PUT --data-binary "first-write-via-${leader_name}" "$url" >/dev/null
echo "  PUT obj-1 OK"

# 5. PUT to a non-leader → should 503 + X-KVFS-Primary
echo
echo "--- step 3: PUT to non-leader → expect 503 + X-KVFS-Primary header ---"
non_leader_port=""
for i in 0 1 2; do
  if [ "${EDGE_NAMES[$i]}" != "$leader_name" ]; then
    non_leader_port="${EDGE_PORTS[$i]}"; non_leader_name="${EDGE_NAMES[$i]}"
    break
  fi
done
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/should-redirect" --ttl 5m --base "http://localhost:${non_leader_port}")
hdr=$(curl -s -o /dev/null -D - -X PUT --data-binary "x" "$url" || true)
echo "  ${non_leader_name} response:"
echo "$hdr" | grep -iE "^HTTP|^X-Kvfs-Primary" | head -3 | awk '{print "    " $0}'

# 6. KILL the leader
echo
echo "--- step 4: kill leader ($leader_name) ---"
docker kill "$leader_name" >/dev/null
echo "  $leader_name killed; waiting for failover..."
sleep 4

# 7. Find new leader
echo
echo "--- step 5: new leader after failover ---"
new_leader_url=""
for i in 0 1 2; do
  port="${EDGE_PORTS[$i]}"
  name="${EDGE_NAMES[$i]}"
  if [ "$name" = "$leader_name" ]; then continue; fi
  state=$(curl -fsS "http://localhost:${port}/v1/election/state" 2>/dev/null || echo '{}')
  s=$(echo "$state" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('state','?'), 'term=', d.get('current_term','?'))" 2>/dev/null || echo "?")
  echo "  ${name} (port ${port}): $s"
  st=$(echo "$state" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('state',''))" 2>/dev/null)
  if [ "$st" = "leader" ]; then
    new_leader_url="http://localhost:${port}"
    new_leader_name="${name}"
  fi
done
[ -z "$new_leader_url" ] && { echo "❌ no new leader after failover"; exit 1; }
echo "  → new leader: $new_leader_name @ $new_leader_url"

# 8. PUT 2 via new leader (proves writes resumed)
echo
echo "--- step 6: PUT obj-2 via new leader ($new_leader_name) ---"
sleep 2  # let the new leader's bbolt sync
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/obj-2" --ttl 5m --base "$new_leader_url")
curl -fsS -X PUT --data-binary "second-write-via-${new_leader_name}-after-failover" "$url" >/dev/null
echo "  PUT obj-2 OK"

# cleanup
echo
for n in edge1 edge2 edge3; do
  docker rm -f "$n" >/dev/null 2>&1 || true
  docker volume rm "${n}-data" >/dev/null 2>&1 || true
done

echo
echo "✅ υ demo PASS — auto-failover within ~4s, writes resumed via new leader"
