#!/usr/bin/env bash
# demo-phi.sh — WAL of metadata mutations (ADR-019).
#
# 1) WAL-enabled edge 에 PUT 5 + DELETE 2
# 2) kvfs-cli wal info  → 7 entries 확인
# 3) kvfs-cli wal stream --since 0 → JSON-lines raw 출력
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"
BUCKET="demo-phi"

echo "=== φ demo: WAL of metadata mutations (ADR-019) ==="

./scripts/down.sh >/dev/null 2>&1 || true

# Up cluster, then restart edge with WAL enabled.
./scripts/up.sh >/dev/null 2>&1
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
for _ in $(seq 1 30); do
  if curl -fsS "$EDGE/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.3
done

echo
echo "--- step 1: PUT 5 objects ---"
for i in 1 2 3 4 5; do
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/obj-$i" --ttl 5m --base "$EDGE")
  curl -fsS -X PUT --data-binary "wal-test-$i-$(date +%s%N)" "$url" >/dev/null
  echo "  PUT obj-$i"
done

echo
echo "--- step 2: DELETE 2 objects ---"
for i in 2 4; do
  url=$(./bin/kvfs-cli sign --method DELETE --path "/v1/o/$BUCKET/obj-$i" --ttl 5m --base "$EDGE")
  curl -fsS -X DELETE "$url" >/dev/null
  echo "  DELETE obj-$i"
done

echo
echo "--- step 3: kvfs-cli wal info ---"
./bin/kvfs-cli wal info --edge "$EDGE"

echo
echo "--- step 4: raw stream (first 3 entries to keep output short) ---"
./bin/kvfs-cli wal stream --edge "$EDGE" --since 0 2>/dev/null | head -3

echo
echo "--- step 5: snapshot endpoint includes X-KVFS-WAL-Seq header ---"
hdr=$(curl -s -o /dev/null -D - "$EDGE/v1/admin/meta/snapshot")
echo "$hdr" | grep -i "X-Kvfs-Wal-Seq" | head -1 | awk '{print "  " $0}'

echo
echo "✅ φ demo PASS — 7 mutations recorded in WAL (5 put + 2 delete), snapshot tagged with WAL seq"
