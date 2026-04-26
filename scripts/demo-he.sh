#!/usr/bin/env bash
# demo-he.sh — Season 5 Ep.5: coord transactional commit (ADR-040).
#
# 3 coord cluster + COORD_TRANSACTIONAL_RAFT=1. Demonstrates true
# replicate-then-commit semantics:
#
#   1. Healthy 3-coord cluster: PUT succeeds (quorum=2 trivially met).
#   2. Kill 2/3 coords → no quorum possible.
#   3. PUT to surviving coord → 503 (NOT 200), and surviving coord's
#      bbolt does NOT contain the entry. (Best-effort mode — ADR-039 —
#      would have committed locally and emitted a phantom write.)
#   4. Restart killed coords → quorum restored → PUT succeeds.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

echo "=== ה he demo: coord transactional commit (Season 5 Ep.5, ADR-040) ==="
echo "    Replicate-then-commit: quorum failure → 503 + leader bbolt untouched."
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

# 3 coords with peers + WAL + transactional commit
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
    -e COORD_TRANSACTIONAL_RAFT=1 \
    -v "coord${i}-data:/var/lib/kvfs-coord" \
    kvfs-coord:dev >/dev/null
done

echo "==> waiting for election (~4s)"
sleep 4

find_leader() {
  for i in 1 2 3; do
    port=$((9000 + i - 1))
    role=$(curl -fs "http://localhost:${port}/v1/coord/healthz" 2>/dev/null | jq -r .role 2>/dev/null || echo "")
    if [ "$role" = "leader" ]; then echo "coord${i}:${port}"; return 0; fi
  done
  return 1
}

LEADER=$(find_leader || true)
[ -n "$LEADER" ] || { echo "FAIL: no leader"; exit 1; }
LEADER_NAME=$(echo "$LEADER" | cut -d: -f1)
LEADER_PORT=$(echo "$LEADER" | cut -d: -f2)
echo "    leader = $LEADER"
echo

# edge in proxy mode pointing at the leader.
docker volume create edge-data >/dev/null
docker run -d --name edge --network $NET -p "8000:8000" \
  -e EDGE_ADDR=":8000" -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_COORD_URL="http://${LEADER_NAME}:9000" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

for _ in $(seq 1 30); do
  curl -fs "http://localhost:8000/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done

# 1. Healthy cluster: PUT must succeed.
echo "==> [healthy 3-coord] PUT he/v1 — expect 200"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket he --key v1 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'healthy commit' \
  "http://localhost:8000/v1/o/he/v1?$SIG" \
  | jq -c '{bucket, key, size}'
echo "    ✓ committed under transactional Raft (quorum trivially met)"
echo

# 2. Kill 2/3 — no quorum possible.
echo "==> killing 2 of 3 coords (leader + one follower) → no quorum"
KILL1="$LEADER_NAME"
# Pick a victim that is NOT the leader, NOT the leader either:
for i in 1 2 3; do
  cand="coord${i}"
  if [ "$cand" != "$LEADER_NAME" ]; then
    KILL2="$cand"
    break
  fi
done
docker kill "$KILL1" "$KILL2" >/dev/null
echo "    killed: $KILL1, $KILL2"
sleep 3

# Find the survivor and point edge at it.
SURVIVOR=""
for i in 1 2 3; do
  cand="coord${i}"
  if [ "$cand" != "$KILL1" ] && [ "$cand" != "$KILL2" ]; then
    SURVIVOR="$cand"
    SURVIVOR_PORT=$((9000 + i - 1))
    break
  fi
done
echo "    survivor = $SURVIVOR (port $SURVIVOR_PORT)"

# Repoint edge at the survivor (not strictly necessary if leader-redirect
# from a follower lands on the survivor, but explicit makes the demo clear).
docker rm -f edge >/dev/null
docker run -d --name edge --network $NET -p "8000:8000" \
  -e EDGE_ADDR=":8000" -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_COORD_URL="http://${SURVIVOR}:9000" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null
for _ in $(seq 1 30); do
  curl -fs "http://localhost:8000/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done

# 3. PUT must 503 (no quorum). The survivor MAY be a leader (if it was
# the previous leader) or a follower (if not). Either way the result is
# 503: follower → leader-redirect to dead leader → eventually fails.
echo
echo "==> [no quorum] PUT he/no-quorum — expect 503 (NOT 200)"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket he --key no-quorum --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
PUT_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PUT --data-binary 'should not commit' \
  "http://localhost:8000/v1/o/he/no-quorum?$SIG" || true)
echo "    PUT status = $PUT_CODE"
case "$PUT_CODE" in
  503|502|500)
    echo "    ✓ rejected (would have been a phantom write under best-effort)"
    ;;
  200|201)
    echo "    FAIL: PUT succeeded under no-quorum — transactional path violated"
    exit 1
    ;;
  *)
    echo "    note: status $PUT_CODE is not 200, treating as expected rejection"
    ;;
esac
echo

# 4. Verify: survivor's bbolt does NOT contain he/no-quorum.
echo "==> verify survivor coord bbolt does NOT contain the rejected key"
LOOKUP_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  "http://localhost:${SURVIVOR_PORT}/v1/coord/lookup?bucket=he&key=no-quorum")
echo "    lookup status = $LOOKUP_CODE"
[ "$LOOKUP_CODE" = "404" ] || { echo "FAIL: phantom write detected (status $LOOKUP_CODE)"; exit 1; }
echo "    ✓ no phantom write — transactional semantics intact"
echo

# 5. Restart killed coords → quorum restored.
echo "==> restarting killed coords"
for victim in "$KILL1" "$KILL2"; do
  docker start "$victim" >/dev/null
done
sleep 6   # election re-stabilize

LEADER2=$(find_leader || true)
[ -n "$LEADER2" ] || { echo "FAIL: no leader after restart"; exit 1; }
echo "    new leader = $LEADER2"
echo

echo "==> [restored quorum] PUT he/v2 — expect 200"
SIG=$(docker run --rm -e EDGE_URLKEY_SECRET="$SECRET" --network $NET kvfs-cli:dev \
  sign --method PUT --bucket he --key v2 --expires-in 60 --base-url http://edge:8000 2>/dev/null \
  | grep -oE 'sig=[^&]+&exp=[0-9]+')
curl -fs -X PUT --data-binary 'restored quorum write' \
  "http://localhost:8000/v1/o/he/v2?$SIG" \
  | jq -c '{bucket, key, size}'
echo

echo "=== ה PASS: transactional commit closes the phantom-write window ==="
echo "    Cleanup: ./scripts/down.sh"
