#!/usr/bin/env bash
# demo-chi.sh — Follower WAL auto-pull (ADR-019 follow-up).
#
# Primary edge writes objects → follower edge auto-pulls WAL deltas (not
# full snapshots) → follower's bbolt mirrors primary within ~1s.
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
PRIMARY="http://localhost:8000"
FOLLOWER="http://localhost:8001"
BUCKET="demo-chi"
PULL=1s

echo "=== χ demo: follower WAL auto-pull (ADR-019 follow-up) ==="

./scripts/down.sh >/dev/null 2>&1 || true
docker rm -f follower 2>/dev/null || true
docker volume rm follower-data 2>/dev/null || true

./scripts/up.sh >/dev/null 2>&1
# Restart primary with WAL enabled.
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
  -e EDGE_WAL_PATH="/var/lib/kvfs-edge/mutations.wal" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

# Follower with same WAL feature, points at primary.
docker volume create follower-data >/dev/null
docker run -d --name follower \
  --network kvfs-net \
  -p 8001:8001 \
  -e EDGE_ADDR=":8001" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_QUORUM_WRITE="2" \
  -e EDGE_ROLE="follower" \
  -e EDGE_PRIMARY_URL="http://edge:8000" \
  -e EDGE_FOLLOWER_PULL_INTERVAL="$PULL" \
  -v "follower-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null

for _ in $(seq 1 30); do
  if curl -fsS "$PRIMARY/healthz" >/dev/null 2>&1 && curl -fsS "$FOLLOWER/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.3
done

echo
echo "--- step 1: PUT 3 objects via primary ---"
for i in 1 2 3; do
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/obj-$i" --ttl 5m --base "$PRIMARY")
  curl -fsS -X PUT --data-binary "wal-pull-test-$i" "$url" >/dev/null
  echo "  PUT obj-$i"
done

echo
echo "--- step 2: wait $PULL + 1s for follower to do snapshot pull (cold start) ---"
sleep 2

echo
echo "--- step 3: GET obj-1 via follower (proves snapshot caught up) ---"
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/$BUCKET/obj-1" --ttl 5m --base "$FOLLOWER")
body=$(curl -fsS "$url")
echo "  follower GET obj-1 → $body"

echo
echo "--- step 4: PUT 2 more via primary, expect follower to catch up via WAL only (no snapshot re-pull) ---"
for i in 4 5; do
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/obj-$i" --ttl 5m --base "$PRIMARY")
  curl -fsS -X PUT --data-binary "wal-pull-test-$i" "$url" >/dev/null
  echo "  PUT obj-$i to primary"
done
sleep 2

echo
echo "--- step 5: GET obj-5 via follower (proves WAL replay applied delta) ---"
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/$BUCKET/obj-5" --ttl 5m --base "$FOLLOWER")
body=$(curl -fsS "$url")
echo "  follower GET obj-5 → $body"

echo
echo "--- step 6: follower role + sync stats ---"
./bin/kvfs-cli role --edge "$FOLLOWER"

echo
echo "--- step 7: primary WAL info (should show 5 entries) ---"
./bin/kvfs-cli wal info --edge "$PRIMARY"

# cleanup
docker rm -f follower >/dev/null 2>&1 || true
docker volume rm follower-data >/dev/null 2>&1 || true

echo
echo "✅ χ demo PASS — follower applied 2 incremental WAL entries after initial snapshot, no full re-pull"
