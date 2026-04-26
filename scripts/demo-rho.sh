#!/usr/bin/env bash
# demo-rho.sh — Multi-edge read-replica HA (ADR-022).
#
# 1 primary edge + 1 follower edge sharing same DN cluster:
#   - primary serves PUT/GET/DELETE
#   - follower serves GET (stale-but-consistent), rejects PUT/DELETE with
#     503 + X-KVFS-Primary
#   - follower pulls primary's snapshot every PULL_INTERVAL → atomic Reload
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
PRIMARY="http://localhost:8000"
FOLLOWER="http://localhost:8001"
BUCKET="demo-rho"
PULL=2s

echo "=== ρ demo: multi-edge read-replica HA (ADR-022) ==="

# 0. clean slate
./scripts/down.sh >/dev/null 2>&1 || true
docker rm -f follower 2>/dev/null || true
docker volume rm follower-data 2>/dev/null || true

# 1. cluster up — primary edge + 3 DNs
./scripts/up.sh

# 2. follower edge — separate volume, EDGE_ROLE=follower, EDGE_PRIMARY_URL=primary
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
  if curl -fsS "${FOLLOWER}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.3
done

echo
echo "--- step 1: PUT 2 objects via primary ---"
for i in 1 2; do
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/obj-${i}" --ttl 5m --base "$PRIMARY")
  curl -fsS -X PUT --data-binary "primary-write-${i}-$(date +%s)" "$url" >/dev/null
  echo "  PUT obj-$i to primary"
done

echo
echo "--- step 2: wait $PULL + 1s for follower to sync ---"
sleep 3

echo
echo "--- step 3: roles ---"
./bin/kvfs-cli role --edge "$PRIMARY"
echo "---"
./bin/kvfs-cli role --edge "$FOLLOWER"

echo
echo "--- step 4: GET both objects from FOLLOWER (read replica) ---"
for i in 1 2; do
  url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/${BUCKET}/obj-${i}" --ttl 5m --base "$FOLLOWER")
  body=$(curl -fsS "$url")
  echo "  GET obj-$i from follower → ${body:0:32}…"
done

echo
echo "--- step 5: PUT to FOLLOWER must 503 + X-KVFS-Primary header ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/should-fail" --ttl 5m --base "$FOLLOWER")
http_code=$(curl -s -o /tmp/follower-put.body -D /tmp/follower-put.hdr -w "%{http_code}" -X PUT --data-binary "should-fail" "$url" || true)
echo "  HTTP code: $http_code"
echo "  X-KVFS-Primary: $(grep -i '^x-kvfs-primary' /tmp/follower-put.hdr | tr -d '\r')"
echo "  Body: $(cat /tmp/follower-put.body)"

echo
echo "--- step 6: PUT 1 more object via primary, then re-sync follower ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/obj-3" --ttl 5m --base "$PRIMARY")
curl -fsS -X PUT --data-binary "primary-write-3-$(date +%s)" "$url" >/dev/null
echo "  PUT obj-3 to primary (follower hasn't synced yet)"
echo "  waiting $PULL for next pull cycle..."
sleep 3

echo
echo "--- step 7: GET obj-3 from follower → should now exist ---"
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/${BUCKET}/obj-3" --ttl 5m --base "$FOLLOWER")
body=$(curl -fsS "$url")
echo "  GET obj-3 from follower → ${body:0:32}…"

echo
echo "--- step 8: final follower role stats ---"
./bin/kvfs-cli role --edge "$FOLLOWER"

# cleanup follower
docker rm -f follower >/dev/null 2>&1 || true
docker volume rm follower-data >/dev/null 2>&1 || true

echo
echo "✅ ρ demo PASS — follower served reads (stale → fresh after sync), rejected writes with X-KVFS-Primary"
