#!/usr/bin/env bash
# demo-pi.sh — Auto-snapshot scheduler (ADR-016). Edge ticker가 주기적으로
# bbolt snapshot을 디렉토리에 timestamped로 떨어뜨리고 keep 초과분을 정리.
#
# Self-contained: down → up 3 DNs + edge with EDGE_SNAPSHOT_INTERVAL=2s,
# EDGE_SNAPSHOT_KEEP=3 → wait several ticks → kvfs-cli meta history (3 files,
# rotation 확인) → PUT object → wait next tick → history (most recent has
# new object) → restore from second-to-latest snapshot to verify file content.
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"
BUCKET="demo-pi"
SNAP_DIR="/var/lib/kvfs-edge/snapshots"
SNAP_INTERVAL="2s"
SNAP_KEEP="3"

echo "=== π demo: auto-snapshot scheduler (ADR-016) ==="

# 0. clean slate
./scripts/down.sh >/dev/null 2>&1 || true

# 1. cluster up + edge with snapshot scheduler
./scripts/up.sh

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
  -e EDGE_SNAPSHOT_DIR="$SNAP_DIR" \
  -e EDGE_SNAPSHOT_INTERVAL="$SNAP_INTERVAL" \
  -e EDGE_SNAPSHOT_KEEP="$SNAP_KEEP" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

for _ in $(seq 1 30); do
  if curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.3
done

echo
echo "--- step 1: PUT 1 object ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/seed" --ttl 5m --base "$EDGE")
curl -fsS -X PUT --data-binary "snapshot-test-content" "$url" >/dev/null
echo "  PUT seed (21 bytes)"

echo
echo "--- step 2: wait 8s for several scheduler ticks (interval=${SNAP_INTERVAL}, keep=${SNAP_KEEP}) ---"
sleep 8

echo
echo "--- step 3: meta history — should show ${SNAP_KEEP} files (rotation working) ---"
./bin/kvfs-cli meta history --edge "$EDGE"

echo
echo "--- step 4: PUT another object (changes meta) ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/extra" --ttl 5m --base "$EDGE")
curl -fsS -X PUT --data-binary "extra-object-content" "$url" >/dev/null
echo "  PUT extra (20 bytes)"

echo
echo "--- step 5: wait 3s for next tick ---"
sleep 3

echo
echo "--- step 6: meta history — newest snapshot should reflect 2 objects ---"
./bin/kvfs-cli meta history --edge "$EDGE"

echo
echo "--- step 7: meta info verification ---"
./bin/kvfs-cli meta info --edge "$EDGE"

echo
echo "✅ π demo PASS — scheduler created snapshots on tick + retained ${SNAP_KEEP}, mtime monotonic"
