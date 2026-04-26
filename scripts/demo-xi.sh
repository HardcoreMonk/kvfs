#!/usr/bin/env bash
# demo-xi.sh — Metadata snapshot + offline restore (ADR-014).
#
# Self-contained: down → up 3 DNs → PUT 3 objects → snapshot → corrupt
# (rm bbolt) → restore from snapshot → GET still works.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"
BUCKET="demo-xi"
SNAPSHOT_PATH="/tmp/kvfs-meta-snapshot-demo-xi.bbolt"

echo "=== ξ demo: meta backup + restore (ADR-014) ==="

. "$(dirname "$0")/lib/common.sh"

need curl; need python3; need docker

# 0. clean slate
./scripts/down.sh >/dev/null 2>&1 || true
rm -f "$SNAPSHOT_PATH"

# 1. cluster up — 3 DNs, plain replication (no EC for simplicity)
./scripts/up.sh

echo
echo "--- step 1: PUT 3 objects ---"
for i in 1 2 3; do
  body="hello-snapshot-$i-$(date +%s)"
  url=$("$(dirname "$0")"/../bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/obj-${i}" --ttl 5m --base "$EDGE")
  curl -fsS -X PUT --data-binary "$body" "$url" >/dev/null
  echo "  PUT obj-$i (${#body} bytes)"
done

echo
echo "--- step 2: meta info before snapshot ---"
./bin/kvfs-cli meta info --edge "$EDGE"

echo
echo "--- step 3: take snapshot ---"
./bin/kvfs-cli meta snapshot --edge "$EDGE" --out "$SNAPSHOT_PATH"
SNAP_SIZE=$(stat -c '%s' "$SNAPSHOT_PATH")
echo "  snapshot size: $SNAP_SIZE bytes"

echo
echo "--- step 4: simulate disaster — stop edge + delete bbolt ---"
docker stop edge >/dev/null
# bbolt lives in the edge-data volume; remove it from inside a throwaway container
docker run --rm -v edge-data:/var/lib/kvfs-edge alpine sh -c 'rm -f /var/lib/kvfs-edge/edge.db' || true
echo "  edge stopped, edge.db removed"

echo
echo "--- step 5: restore snapshot into the volume ---"
# Copy snapshot into the volume as edge.db. kvfs runs as UID 10001 inside the
# edge container, so chown to match before restart.
docker run --rm -v edge-data:/var/lib/kvfs-edge -v "$SNAPSHOT_PATH:/snap.bbolt:ro" alpine \
  sh -c 'cp /snap.bbolt /var/lib/kvfs-edge/edge.db && chown 10001:10001 /var/lib/kvfs-edge/edge.db && chmod 600 /var/lib/kvfs-edge/edge.db'
echo "  snapshot copied to edge-data volume"

echo
echo "--- step 6: restart edge ---"
docker start edge >/dev/null
for _ in $(seq 1 30); do
  if curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done

echo
echo "--- step 7: meta info after restore ---"
./bin/kvfs-cli meta info --edge "$EDGE"

echo
echo "--- step 8: GET each object back ---"
for i in 1 2 3; do
  url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/${BUCKET}/obj-${i}" --ttl 5m --base "$EDGE")
  body=$(curl -fsS "$url")
  echo "  GET obj-$i → ${body:0:32}…"
done

echo
echo "✅ ξ demo PASS — snapshot → simulated disaster → restore → reads recovered"
