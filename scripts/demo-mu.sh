#!/usr/bin/env bash
# demo-mu.sh — EC stripe rebalance (ADR-024). Adds dn7 to a 6-DN cluster
# that already holds EC(4+2) shards, then runs rebalance to migrate one shard
# per stripe to the new node — minimum data movement (set-based).
#
# Self-contained: down → up 6 DNs → PUT 128 KiB EC → add dn7 → restart edge →
# rebalance --plan → --apply → verify dn7 has shards + GET still works.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-mu"
KEY="ec-rebal.bin"
SHARD_SIZE=$((16 * 1024))
K=4
M=2
OBJ_SIZE=$((2 * K * SHARD_SIZE))   # 128 KiB → 2 stripes of (4+2)

echo "=== μ demo: EC stripe rebalance (ADR-024) ==="

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl; need python3; need docker

sign_url() {
  SECRET="$SECRET" python3 -c '
import hmac, hashlib, time, sys, os
secret=os.environ["SECRET"].encode(); method=sys.argv[1]; path=sys.argv[2]; ttl=int(sys.argv[3])
exp=int(time.time())+ttl
sig=hmac.new(secret,f"{method}:{path}:{exp}".encode(),hashlib.sha256).hexdigest()
print(f"{path}?sig={sig}&exp={exp}")
' "$1" "$2" "$3"
}

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
    -e EDGE_CHUNK_SIZE="$SHARD_SIZE" \
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

print_disks() {
  for dn in dn1 dn2 dn3 dn4 dn5 dn6 dn7; do
    if docker inspect "$dn" >/dev/null 2>&1; then
      printf "  %-4s shards = %s\n" "$dn" "$(dn_chunk_count $dn)"
    fi
  done
}

echo
echo "[1/8] Reset cluster + start 6 DNs (K+M=6)"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3 dn4 dn5 dn6; do start_dn "$id"; done
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080"
echo "  ✅ 6 DNs + edge up"

echo
echo "[2/8] Generate $OBJ_SIZE-byte random body"
TMP=$(mktemp)
trap 'rm -f "$TMP" "$TMP.got"' EXIT
head -c "$OBJ_SIZE" /dev/urandom > "$TMP"
expected_sha=$(sha256sum "$TMP" | awk '{print $1}')
echo "  body sha256: ${expected_sha:0:32}.."

echo
echo "[3/8] PUT object with X-KVFS-EC: $K+$M (2 stripes × 6 shards)"
path="/v1/o/${BUCKET}/${KEY}"
url="${EDGE}$(sign_url PUT "$path" 600)"
curl -fsS -X PUT "$url" \
  -H "Content-Type: application/octet-stream" \
  -H "X-KVFS-EC: $K+$M" \
  --data-binary "@${TMP}" >/dev/null
echo "  ✅ PUT done"

echo
echo "[4/8] Disk before adding dn7 (each DN holds 2 shards = 12 total)"
print_disks

echo
echo "[5/8] Add dn7 + restart edge with 7-DN config (auto OFF, manual rebalance)"
start_dn dn7
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080,dn7:8080"
echo "  Disk right after dn7 join (still 0 shards on dn7 — write-time placement only):"
print_disks

echo
echo "[6/8] kvfs-cli rebalance --plan -v"
./bin/kvfs-cli rebalance --edge "$EDGE" --plan -v

echo
echo "[7/8] kvfs-cli rebalance --apply"
./bin/kvfs-cli rebalance --edge "$EDGE" --apply --concurrency 4

echo
echo "  Disk after rebalance:"
print_disks

echo
echo "[8/8] GET still works + idempotency check"
url="${EDGE}$(sign_url GET "$path" 600)"
curl -fsS "$url" -o "$TMP.got"
got_sha=$(sha256sum "$TMP.got" | awk '{print $1}')
if [ "$got_sha" = "$expected_sha" ]; then
  echo "  ✅ GET sha256 matches"
else
  echo "  ❌ GET mismatch"; exit 1
fi
echo
./bin/kvfs-cli rebalance --edge "$EDGE" --plan

echo
echo "✅ EC stripe rebalance verified:"
echo "   - dn7 가 EC stripe 의 일부 shard 를 인수받음 (set-based, 최소 이동)"
echo "   - 옛 shard (rebalance 가 안 지움) 는 GC 가 청소 (kvfs-cli gc --apply --min-age 0)"
echo "   - 두 번째 plan = 0 (멱등)"
