#!/usr/bin/env bash
# demo-iota.sh — chunking (ADR-011): a 256 KiB object with EDGE_CHUNK_SIZE=64 KiB
# becomes 4 independent chunks, each placed via HRW on its own R-DN set.
#
# Self-contained: tears down → up edge with EDGE_CHUNK_SIZE=65536 → PUT 256 KiB
# random body → confirm 4 chunks + each chunk's replicas + GET round-trip.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-iota"
KEY="big-blob.bin"
CHUNK_SIZE=65536          # 64 KiB
OBJ_SIZE=$((4 * CHUNK_SIZE))   # 256 KiB → exactly 4 chunks

echo "=== ι demo: chunking (ADR-011) ==="

. "$(dirname "$0")/lib/common.sh"

need curl; need python3; need docker
if ! command -v jq >/dev/null; then
  echo "warning: jq missing, output will be less pretty"
fi
pp() { if command -v jq >/dev/null; then jq .; else cat; fi; }


cluster_up() {
  local dn_csv="$1"
  docker rm -f edge >/dev/null 2>&1 || true
  docker run -d --name edge \
    --network "$NET" \
    -p 8000:8000 \
    -e EDGE_ADDR=":8000" \
    -e EDGE_DNS="$dn_csv" \
    -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
    -e EDGE_URLKEY_SECRET="$SECRET" \
    -e EDGE_QUORUM_WRITE="2" \
    -e EDGE_CHUNK_SIZE="$CHUNK_SIZE" \
    -v "edge-data:/var/lib/kvfs-edge" \
    kvfs-edge:dev >/dev/null
  for _ in $(seq 1 30); do
    if curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then return; fi
    sleep 0.5
  done
  echo "edge failed to come up"; exit 1
}

start_dn() {
  local id="$1"
  docker volume inspect "${id}-data" >/dev/null 2>&1 || docker volume create "${id}-data" >/dev/null
  docker rm -f "$id" >/dev/null 2>&1 || true
  docker run -d --name "$id" \
    --network "$NET" \
    -e DN_ID="$id" \
    -e DN_ADDR=":8080" \
    -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "${id}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
}

dn_chunk_count() {
  docker exec "$1" sh -c 'find /var/lib/kvfs-dn/chunks -type f 2>/dev/null | wc -l' | tr -d ' '
}

echo
echo "[1/6] Reset cluster with EDGE_CHUNK_SIZE=$CHUNK_SIZE bytes (64 KiB)"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3 dn4; do start_dn "$id"; done
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080"
echo "  ✅ 4 DNs + edge up (chunk_size=$CHUNK_SIZE)"

echo
echo "[2/6] Generate $OBJ_SIZE-byte random body"
TMP=$(mktemp)
trap 'rm -f "$TMP" "$TMP.got"' EXIT
head -c "$OBJ_SIZE" /dev/urandom > "$TMP"
expected_sha=$(sha256sum "$TMP" | awk '{print $1}')
echo "  body bytes:  $(stat -c%s "$TMP")"
echo "  body sha256: ${expected_sha:0:32}.."

echo
echo "[3/6] PUT object (expecting $((OBJ_SIZE / CHUNK_SIZE)) chunks)"
path="/v1/o/${BUCKET}/${KEY}"
url="${EDGE}$(sign_url PUT "$path" 600)"
resp=$(curl -fsS -X PUT "$url" \
  -H "Content-Type: application/octet-stream" \
  --data-binary "@${TMP}")
echo "$resp" | pp

n_chunks=$(echo "$resp" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["chunks"]))')
echo
echo "  → server reported $n_chunks chunks"
if [ "$n_chunks" -ne 4 ]; then
  echo "  ❌ expected 4 chunks (object 256 KiB / chunk 64 KiB)"; exit 1
fi

echo
echo "[4/6] Per-chunk replica placement (each chunk independently HRW-placed)"
echo "$resp" | python3 -c '
import json, sys
chunks = json.load(sys.stdin)["chunks"]
for i, c in enumerate(chunks):
    cid = c["chunk_id"][:16]
    sz  = c["size"]
    rep = c["replicas"]
    print("  chunk[%d]  id=%s..  size=%6d  replicas=%s" % (i, cid, sz, rep))
'

echo
echo "[5/6] GET object → reassemble + integrity check (sha256)"
url="${EDGE}$(sign_url GET "$path" 600)"
curl -fsS "$url" -o "$TMP.got"
got_sha=$(sha256sum "$TMP.got" | awk '{print $1}')
got_size=$(stat -c%s "$TMP.got")
echo "  got bytes:   $got_size  (want $OBJ_SIZE)"
echo "  got sha256:  ${got_sha:0:32}.."
if [ "$got_sha" = "$expected_sha" ] && [ "$got_size" -eq "$OBJ_SIZE" ]; then
  echo "  ✅ round-trip identical"
else
  echo "  ❌ mismatch"; exit 1
fi

echo
echo "[6/6] Disk distribution across 4 DNs"
total=0
for dn in dn1 dn2 dn3 dn4; do
  c=$(dn_chunk_count "$dn")
  total=$((total + c))
  printf "  %-4s chunk count = %s\n" "$dn" "$c"
done
echo "  total replica copies on disk: $total  (expected 4 chunks × 3 replicas = 12)"

echo
echo "✅ Chunking verified:"
echo "   - 256 KiB object split into 4 × 64 KiB chunks (last may be smaller for non-multiples)"
echo "   - each chunk_id placed independently via Rendezvous Hashing"
echo "   - GET reassembles in order with per-chunk sha256 integrity check"
echo "   - rebalance/gc unchanged (already chunk-unit algorithms)"
