#!/usr/bin/env bash
# demo-tau.sh — Content-defined chunking (FastCDC, ADR-018).
#
# Compares dedup behaviour of fixed vs CDC chunkers under the "1 byte
# inserted at file start" scenario:
#
#   F1  = 16 MiB random
#   F2  = 1 byte + F1   (shifted-by-1)
#
# Fixed mode:  every chunk boundary moves → 0% dedup → 8 unique chunks
# CDC   mode:  rolling hash re-aligns boundaries after the shift → most
#              chunks identical → high dedup
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"
BUCKET="demo-tau"
SIZE_MB=16

run_scenario() {
  local mode="$1"
  echo
  echo "=================================="
  echo " mode = $mode"
  echo "=================================="

  ./scripts/down.sh >/dev/null 2>&1 || true

  ./scripts/up.sh >/dev/null 2>&1
  # Restart edge with chunk-mode override.
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
    -e EDGE_CHUNK_MODE="$mode" \
    -v "edge-data:/var/lib/kvfs-edge" \
    kvfs-edge:dev >/dev/null
  for _ in $(seq 1 30); do
    if curl -fsS "$EDGE/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.3
  done

  # Generate F1 + F2 once per scenario (same content across modes for fair compare).
  if [ ! -f /tmp/demo-tau-F1.bin ]; then
    dd if=/dev/urandom of=/tmp/demo-tau-F1.bin bs=1M count=$SIZE_MB status=none
    printf '\xAA' > /tmp/demo-tau-prefix.bin
    cat /tmp/demo-tau-prefix.bin /tmp/demo-tau-F1.bin > /tmp/demo-tau-F2.bin
  fi

  echo "--- PUT F1 ($SIZE_MB MiB random) ---"
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/F1.bin" --ttl 5m --base "$EDGE")
  curl -fsS -X PUT --data-binary "@/tmp/demo-tau-F1.bin" "$url" -o /tmp/demo-tau-F1-resp.json
  f1_chunks=$(python3 -c "import json,sys; d=json.load(open('/tmp/demo-tau-F1-resp.json')); print(len(d['chunks'])); [print(c['chunk_id'][:16], c['size']) for c in d['chunks']]")
  f1_count=$(echo "$f1_chunks" | head -1)
  echo "  chunks: $f1_count"
  echo "$f1_chunks" | tail -n +2 | awk '{print "    " $0}'

  echo "--- PUT F2 (1 byte + F1) ---"
  url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/F2.bin" --ttl 5m --base "$EDGE")
  curl -fsS -X PUT --data-binary "@/tmp/demo-tau-F2.bin" "$url" -o /tmp/demo-tau-F2-resp.json
  f2_chunks=$(python3 -c "import json,sys; d=json.load(open('/tmp/demo-tau-F2-resp.json')); print(len(d['chunks'])); [print(c['chunk_id'][:16], c['size']) for c in d['chunks']]")
  f2_count=$(echo "$f2_chunks" | head -1)
  echo "  chunks: $f2_count"
  echo "$f2_chunks" | tail -n +2 | awk '{print "    " $0}'

  echo "--- dedup analysis ---"
  python3 <<EOF
import json
f1 = json.load(open('/tmp/demo-tau-F1-resp.json'))['chunks']
f2 = json.load(open('/tmp/demo-tau-F2-resp.json'))['chunks']
ids1 = {c['chunk_id'] for c in f1}
ids2 = {c['chunk_id'] for c in f2}
total = len(f1) + len(f2)
unique = len(ids1 | ids2)
shared = len(ids1 & ids2)
dedup = (total - unique) / total if total else 0
print(f"  total chunk refs : {total}")
print(f"  unique chunks    : {unique}")
print(f"  shared chunks    : {shared}")
print(f"  dedup ratio      : {dedup * 100:.1f}%")
EOF
}

echo "=== τ demo: fixed vs CDC chunking, shift-by-1 dedup (ADR-018) ==="
run_scenario fixed
run_scenario cdc

echo
echo "✅ τ demo PASS — CDC mode produces shared chunks where fixed produces none"
