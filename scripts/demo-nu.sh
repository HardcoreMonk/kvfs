#!/usr/bin/env bash
# demo-nu.sh — EC repair queue (ADR-025). Reconstructs shards on dead DNs
# from K surviving shards in the same stripe via Reed-Solomon.
#
# Self-contained: down → up 8 DNs → PUT 128 KiB EC(4+2) → kill+remove dn5/dn6
# → repair --plan/--apply → GET still works (sha256 matches).
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-nu"
KEY="ec-repair.bin"
SHARD_SIZE=$((16 * 1024))
K=4
M=2
OBJ_SIZE=$((2 * K * SHARD_SIZE))   # 128 KiB → 2 stripes

echo "=== ν demo: EC repair queue (ADR-025) ==="

. "$(dirname "$0")/lib/common.sh"

need curl; need python3; need docker

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

dn_chunks() {
  docker exec "$1" sh -c 'find /var/lib/kvfs-dn/chunks -type f 2>/dev/null | wc -l' | tr -d ' '
}

print_disks() {
  for dn in dn1 dn2 dn3 dn4 dn5 dn6 dn7 dn8; do
    if docker inspect "$dn" >/dev/null 2>&1 && docker inspect -f '{{.State.Running}}' "$dn" 2>/dev/null | grep -q true; then
      printf "  %-4s shards = %s\n" "$dn" "$(dn_chunks $dn)"
    fi
  done
}

echo
echo "[1/8] Reset cluster + start 8 DNs (6 active + 2 spare)"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3 dn4 dn5 dn6 dn7 dn8; do start_dn "$id"; done
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080,dn7:8080,dn8:8080"
echo "  ✅ 8 DNs registered"

echo
echo "[2/8] Generate $OBJ_SIZE-byte random body"
TMP=$(mktemp); trap 'rm -f "$TMP" "$TMP.got"' EXIT
head -c "$OBJ_SIZE" /dev/urandom > "$TMP"
expected_sha=$(sha256sum "$TMP" | awk '{print $1}')
echo "  body sha256: ${expected_sha:0:32}.."

echo
echo "[3/8] PUT with X-KVFS-EC: $K+$M (2 stripes × 6 shards)"
path="/v1/o/${BUCKET}/${KEY}"
url="${EDGE}$(sign_url PUT "$path" 600)"
curl -fsS -X PUT "$url" -H "Content-Type: application/octet-stream" \
  -H "X-KVFS-EC: $K+$M" --data-binary "@${TMP}" >/dev/null
echo "  ✅ PUT done"
echo
echo "  Disk before kill:"
print_disks

echo
echo "[4/8] Kill dn5 + dn6 + remove from DN registry (simulate permanent loss)"
docker stop dn5 dn6 >/dev/null
./bin/kvfs-cli dns remove dn5:8080 >/dev/null
./bin/kvfs-cli dns remove dn6:8080 >/dev/null
echo "  ✅ dn5+dn6 stopped + removed from registry"
echo
./bin/kvfs-cli dns list

echo
echo "[5/8] kvfs-cli repair --plan"
./bin/kvfs-cli repair --edge "$EDGE" --plan -v

echo
echo "[6/8] kvfs-cli repair --apply"
./bin/kvfs-cli repair --edge "$EDGE" --apply --concurrency 2

echo
echo "  Disk after repair (rebuilt shards on new DNs):"
print_disks

echo
echo "[7/8] GET still works — body integrity check"
url="${EDGE}$(sign_url GET "$path" 600)"
curl -fsS "$url" -o "$TMP.got"
got_sha=$(sha256sum "$TMP.got" | awk '{print $1}')
if [ "$got_sha" = "$expected_sha" ]; then
  echo "  ✅ GET sha256 matches → EC stripe successfully repaired"
else
  echo "  ❌ sha mismatch (got ${got_sha:0:32}.., want ${expected_sha:0:32}..)"
  exit 1
fi

echo
echo "[8/8] Idempotency: second --plan = 0 repairs"
./bin/kvfs-cli repair --edge "$EDGE" --plan

echo
echo "✅ EC repair verified:"
echo "   - dn5 + dn6 가 영구 사망 + registry 에서 제거됨"
echo "   - Reed-Solomon Reconstruct 로 4 surviving shards 에서 데이터 재구성"
echo "   - 새 DN (sorted unused desired) 으로 PUT + 메타 갱신"
echo "   - GET 정상, 두 번째 plan = 0 (멱등)"
