#!/usr/bin/env bash
# demo-sigma.sh — Streaming PUT/GET (ADR-017). 64 MiB random body PUT, GET 후
# sha256 round-trip 일치 + edge 메모리 사용량 (peak heap) 측정.
#
# Self-contained: down → up 3 DNs → PUT 64 MiB → GET → sha256 비교 → docker
# stats 로 edge 컨테이너 메모리 확인.
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"
BUCKET="demo-sigma"
KEY="big-blob.bin"
SIZE_MB=64

echo "=== σ demo: streaming PUT/GET (ADR-017) ==="

# 0. clean slate
./scripts/down.sh >/dev/null 2>&1 || true
rm -f /tmp/demo-sigma-{src,dl}.bin

# 1. cluster up (default 4 MiB chunkSize → 64 MiB → 16 chunks)
./scripts/up.sh

echo
echo "--- step 1: generate $SIZE_MB MiB random body ---"
dd if=/dev/urandom of=/tmp/demo-sigma-src.bin bs=1M count=$SIZE_MB status=none
SRC_SHA=$(sha256sum /tmp/demo-sigma-src.bin | awk '{print $1}')
echo "  src sha256: $SRC_SHA"

echo
echo "--- step 2: PUT (streaming — edge holds at most chunkSize at a time) ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/${BUCKET}/${KEY}" --ttl 5m --base "$EDGE")
t0=$(date +%s.%N)
curl -fsS -X PUT --data-binary "@/tmp/demo-sigma-src.bin" "$url" -o /tmp/demo-sigma-put.json
t1=$(date +%s.%N)
elapsed=$(echo "$t1 - $t0" | bc)
chunks=$(python3 -c "import json; print(len(json.load(open('/tmp/demo-sigma-put.json'))['chunks']))")
echo "  chunks created: $chunks"
echo "  PUT elapsed: ${elapsed}s"

echo
echo "--- step 3: edge memory snapshot during PUT (docker stats) ---"
docker stats --no-stream --format 'table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' edge

echo
echo "--- step 4: GET (streaming — chunks arrive sequentially) ---"
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/${BUCKET}/${KEY}" --ttl 5m --base "$EDGE")
t0=$(date +%s.%N)
curl -fsS "$url" -o /tmp/demo-sigma-dl.bin
t1=$(date +%s.%N)
elapsed=$(echo "$t1 - $t0" | bc)
DL_SHA=$(sha256sum /tmp/demo-sigma-dl.bin | awk '{print $1}')
echo "  GET elapsed: ${elapsed}s"
echo "  dl  sha256: $DL_SHA"

echo
if [ "$SRC_SHA" = "$DL_SHA" ]; then
  echo "✅ σ demo PASS — $SIZE_MB MiB round-trip sha256 일치, edge mem peak 위 docker stats 값 (chunkSize 4 MiB ≪ object 64 MiB)"
else
  echo "❌ σ demo FAIL — sha mismatch ($SRC_SHA != $DL_SHA)"
  exit 1
fi
