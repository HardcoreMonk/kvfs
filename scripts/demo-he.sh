#!/usr/bin/env bash
# demo-he.sh — Season 5 Ep.5: coord transactional commit (ADR-040).
#
# 3 coord cluster + COORD_TRANSACTIONAL_RAFT=1. Replicate-then-commit:
# quorum failure → 503 + leader bbolt untouched (no phantom write).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
PEERS_INTERNAL="http://coord1:9000,http://coord2:9000,http://coord3:9000"

echo "=== ה he demo: coord transactional commit (Season 5 Ep.5, ADR-040) ==="
echo "    Replicate-then-commit: quorum failure → 503 + leader bbolt untouched."

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
    "/var/lib/kvfs-coord/coord.wal" "1"
done

echo "==> waiting for election (~4s)"
sleep 4

LEADER=$(find_coord_leader 3 || true)
[ -n "$LEADER" ] || { echo "FAIL: no leader"; exit 1; }
LEADER_NAME=$(echo "$LEADER" | cut -d: -f1)
echo "    leader = $LEADER"

start_edge edge 8000 "http://${LEADER_NAME}:9000"
wait_healthz "${EDGE}/healthz"
echo

# 1. Healthy cluster: PUT must succeed.
put_url="${EDGE}$(sign_url PUT /v1/o/he/v1 60)"
echo "==> [healthy 3-coord] PUT he/v1 — expect 200"
curl -fs -X PUT --data-binary 'healthy commit' "$put_url" \
  | jq -c '{bucket, key, size}'
echo "    ✓ committed under transactional Raft"
echo

# 2. Kill 2/3 — no quorum.
echo "==> killing 2 of 3 coords (leader + one follower) → no quorum"
KILL1="$LEADER_NAME"
KILL2=""
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

# Find survivor + repoint edge.
SURVIVOR=""; SURVIVOR_PORT=""
for i in 1 2 3; do
  cand="coord${i}"
  if [ "$cand" != "$KILL1" ] && [ "$cand" != "$KILL2" ]; then
    SURVIVOR="$cand"
    SURVIVOR_PORT=$((9000 + i - 1))
    break
  fi
done
echo "    survivor = $SURVIVOR (port $SURVIVOR_PORT)"

docker rm -f edge >/dev/null
start_edge edge 8000 "http://${SURVIVOR}:9000"
wait_healthz "${EDGE}/healthz"
echo

# 3. PUT must 503 (no quorum).
put_url="${EDGE}$(sign_url PUT /v1/o/he/no-quorum 60)"
echo "==> [no quorum] PUT he/no-quorum — expect 5xx (NOT 200)"
PUT_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PUT --data-binary 'should not commit' "$put_url" || true)
echo "    PUT status = $PUT_CODE"
case "$PUT_CODE" in
  503|502|500) echo "    ✓ rejected (would have been phantom write under best-effort)" ;;
  200|201)     echo "    FAIL: PUT succeeded under no-quorum"; exit 1 ;;
  *)           echo "    note: status $PUT_CODE not 200 — treating as expected rejection" ;;
esac
echo

# 4. Verify survivor's bbolt does NOT contain the rejected key.
echo "==> verify survivor bbolt does NOT contain the rejected key"
LOOKUP_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  "http://localhost:${SURVIVOR_PORT}/v1/coord/lookup?bucket=he&key=no-quorum")
echo "    lookup status = $LOOKUP_CODE"
[ "$LOOKUP_CODE" = "404" ] || { echo "FAIL: phantom write detected"; exit 1; }
echo "    ✓ no phantom write — transactional semantics intact"
echo

# 5. Restart killed coords → quorum restored.
echo "==> restarting killed coords"
for victim in "$KILL1" "$KILL2"; do
  docker start "$victim" >/dev/null
done
sleep 6

LEADER2=$(find_coord_leader 3 || true)
[ -n "$LEADER2" ] || { echo "FAIL: no leader after restart"; exit 1; }
echo "    new leader = $LEADER2"

put_url="${EDGE}$(sign_url PUT /v1/o/he/v2 60)"
echo "==> [restored quorum] PUT he/v2 — expect 200"
curl -fs -X PUT --data-binary 'restored quorum write' "$put_url" \
  | jq -c '{bucket, key, size}'

echo
echo "=== ה PASS: transactional commit closes the phantom-write window ==="
echo "    Cleanup: ./scripts/down.sh"
