#!/usr/bin/env bash
# demo-kappa.sh — Reed-Solomon Erasure Coding (ADR-008): same M failure
# tolerance as 3-way replication, but with K+M=4+2 → only 50% disk overhead.
#
# Self-contained: tears down → up 6 DNs → PUT 128 KiB with X-KVFS-EC: 4+2
# (2 stripes × 6 shards = 12 shards) → kill 2 DNs → GET reconstructs from
# the surviving 4 shards per stripe.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-kappa"
KEY="ec-blob.bin"
SHARD_SIZE=$((16 * 1024))    # 16 KiB shard
K=4
M=2
# 1 stripe = K * SHARD_SIZE = 64 KiB → use 128 KiB body for 2 stripes.
OBJ_SIZE=$((2 * K * SHARD_SIZE))   # 128 KiB

echo "=== κ demo: Reed-Solomon EC (K=$K, M=$M) — ADR-008 ==="

. "$(dirname "$0")/lib/common.sh"

need curl; need python3; need docker
if ! command -v jq >/dev/null; then
  echo "warning: jq missing, output less pretty"
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

echo
echo "[1/7] Reset cluster with 6 DNs (K+M=$((K+M)) needs 6 distinct DNs)"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3 dn4 dn5 dn6; do start_dn "$id"; done
DNS_ALL="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080"
cluster_up "$DNS_ALL"
echo "  ✅ 6 DNs + edge up (chunk_size=$SHARD_SIZE)"

echo
echo "[2/7] Generate $OBJ_SIZE-byte random body (= 2 stripes of K=$K × $SHARD_SIZE)"
TMP=$(mktemp)
trap 'rm -f "$TMP" "$TMP.got"' EXIT
head -c "$OBJ_SIZE" /dev/urandom > "$TMP"
expected_sha=$(sha256sum "$TMP" | awk '{print $1}')
echo "  body bytes:  $(stat -c%s "$TMP")"
echo "  body sha256: ${expected_sha:0:32}.."

echo
echo "[3/7] PUT object with X-KVFS-EC: $K+$M"
path="/v1/o/${BUCKET}/${KEY}"
url="${EDGE}$(sign_url PUT "$path" 600)"
resp=$(curl -fsS -X PUT "$url" \
  -H "Content-Type: application/octet-stream" \
  -H "X-KVFS-EC: $K+$M" \
  --data-binary "@${TMP}")

echo "$resp" | python3 -c '
import json, sys
r = json.load(sys.stdin)
print("  ec:", r["ec"])
print("  num_stripes:", r["num_stripes"])
print()
for si, st in enumerate(r["stripes"]):
    print("  stripe[%d] id=%s.." % (si, st["stripe_id"][:16]))
    for sh_i, sh in enumerate(st["shards"]):
        kind = "data " if sh_i < r["ec"]["k"] else "parity"
        print("    shard[%d] %s id=%s.. size=%d → %s" % (
            sh_i, kind, sh["chunk_id"][:16], sh["size"], sh["replicas"][0]))
'

echo
echo "[4/7] GET (no failures yet) → integrity round-trip"
url="${EDGE}$(sign_url GET "$path" 600)"
curl -fsS "$url" -o "$TMP.got"
got_sha=$(sha256sum "$TMP.got" | awk '{print $1}')
if [ "$got_sha" = "$expected_sha" ]; then
  echo "  ✅ healthy GET sha256 matches"
else
  echo "  ❌ healthy GET mismatch"; exit 1
fi

echo
echo "[5/7] Disk distribution across 6 DNs (each shard on exactly 1 DN)"
total=0
for dn in dn1 dn2 dn3 dn4 dn5 dn6; do
  c=$(dn_chunk_count "$dn")
  total=$((total + c))
  printf "  %-4s shards = %s\n" "$dn" "$c"
done
echo "  total: $total  (expected 2 stripes × $((K+M)) shards = $((2*(K+M))))"

echo
echo "[6/7] Kill dn5 + dn6 (M=$M failures, the maximum tolerated) → GET still works"
docker stop dn5 dn6 >/dev/null
sleep 1
url="${EDGE}$(sign_url GET "$path" 600)"
curl -fsS "$url" -o "$TMP.got"
got_sha=$(sha256sum "$TMP.got" | awk '{print $1}')
if [ "$got_sha" = "$expected_sha" ]; then
  echo "  ✅ GET succeeded after 2 DN kills — Reed-Solomon Reconstruct rebuilt missing shards"
  echo "     reconstructed sha256: ${got_sha:0:32}.."
else
  echo "  ❌ post-kill GET mismatch"
  docker start dn5 dn6 >/dev/null
  exit 1
fi

echo
echo "[7/7] Restore dn5 + dn6 + sanity check"
docker start dn5 dn6 >/dev/null
sleep 1
url="${EDGE}$(sign_url GET "$path" 600)"
curl -fsS "$url" -o "$TMP.got"
got_sha=$(sha256sum "$TMP.got" | awk '{print $1}')
if [ "$got_sha" = "$expected_sha" ]; then
  echo "  ✅ post-restore GET still matches"
else
  echo "  ❌ unexpected mismatch after restore"
  exit 1
fi

echo
echo "✅ Reed-Solomon EC verified:"
echo "   - 128 KiB object split into 2 stripes × $((K+M)) shards each"
echo "   - each shard on a distinct DN (placement.Pick(stripe_id, K+M))"
echo "   - storage overhead = ${M}/${K} = $((100 * M / K))% (vs replication R=3 = 200%)"
echo "   - tolerated $M simultaneous DN failures (= M)"
echo "   - reconstructed via Gauss-Jordan over GF(2^8) on K surviving shards"
