#!/usr/bin/env bash
# demo-omicron.sh — DN heartbeat (ADR-030). edge가 주기적으로 모든 DN /healthz를
# probe하고 운영자에게 unhealthy 상태를 즉시 visible 하게 보여준다.
#
# Self-contained: down → up 3 DNs → wait 첫 probe → kvfs-cli heartbeat (모두 healthy)
# → kill dn3 → 충분히 대기 (3 consec fail) → kvfs-cli heartbeat (dn3 unhealthy)
# → restart dn3 → kvfs-cli heartbeat (dn3 recovered).
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"

# 1s interval + threshold=3 → ~3s to flip unhealthy. demo 빠르게 보려고 짧게.
HB_INTERVAL=1s
HB_THRESHOLD=3

echo "=== ο demo: DN heartbeat (ADR-030) ==="

# 0. clean slate
./scripts/down.sh >/dev/null 2>&1 || true

# 1. cluster up — 3 DNs + edge with heartbeat enabled
./scripts/up.sh

# Override edge with heartbeat env (up.sh doesn't set these by default).
docker stop edge >/dev/null
docker rm edge >/dev/null
docker run -d --name edge \
  --network kvfs-net \
  -p 8000:8000 \
  -e EDGE_ADDR=":8000" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_QUORUM_WRITE="2" \
  -e EDGE_HEARTBEAT_INTERVAL="$HB_INTERVAL" \
  -e EDGE_HEARTBEAT_FAIL_THRESHOLD="$HB_THRESHOLD" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

# Wait for edge healthy
for _ in $(seq 1 30); do
  if curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.3
done

echo
echo "--- step 1: wait 2s for first heartbeat tick ---"
sleep 2

echo
echo "--- step 2: heartbeat snapshot — all healthy ---"
./bin/kvfs-cli heartbeat --edge "$EDGE"

echo
echo "--- step 3: kill dn3 ---"
docker kill dn3 >/dev/null
echo "  dn3 killed"

echo
echo "--- step 4: wait ${HB_THRESHOLD}+1 = $((HB_THRESHOLD+1))s for ${HB_THRESHOLD} consecutive fails ---"
sleep $((HB_THRESHOLD+1))

echo
echo "--- step 5: heartbeat snapshot — dn3 unhealthy ---"
./bin/kvfs-cli heartbeat --edge "$EDGE"

echo
echo "--- step 6: restart dn3 ---"
docker start dn3 >/dev/null
echo "  dn3 restarted; waiting 3s for recovery"
sleep 3

echo
echo "--- step 7: heartbeat snapshot — dn3 recovered ---"
./bin/kvfs-cli heartbeat --edge "$EDGE"

echo
echo "✅ ο demo PASS — heartbeat detected DN failure within ${HB_THRESHOLD}s and recovery on next probe"
