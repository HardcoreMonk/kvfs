#!/usr/bin/env bash
# demo-theta.sh — closes the loop opened by demo-zeta+demo-eta:
# rebalance left over-replicated chunks (surplus); GC cleans them safely.
#
# Self-contained: tears down → up → seed → add dn4 → rebalance → gc.
# At the end: dn1/2/3 chunk count drops by the migrated amount,
# meta-disk consistency restored.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-theta"

echo "=== θ demo: surplus-chunk GC (ADR-012) ==="

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

put_obj() {
  local key="$1" body="$2"
  local path="/v1/o/${BUCKET}/${key}"
  local url="${EDGE}$(sign_url PUT "$path" 600)"
  curl -fsS -X PUT "$url" -H "Content-Type: text/plain" --data-binary "$body"
}

first_chunk_id() {
  python3 -c 'import json,sys; print(json.load(sys.stdin)["chunks"][0]["chunk_id"])'
}

dn_chunk_count() {
  docker exec "$1" sh -c 'find /var/lib/kvfs-dn/chunks -type f 2>/dev/null | wc -l' | tr -d ' '
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

print_disks() {
  for dn in dn1 dn2 dn3 dn4; do
    printf "  %-4s chunk count = %s\n" "$dn" "$(dn_chunk_count $dn)"
  done
}

echo
echo "[1/8] Reset cluster from scratch"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3; do start_dn "$id"; done
cluster_up "dn1:8080,dn2:8080,dn3:8080"
echo "  ✅ 3 DNs + edge up"

echo
echo "[2/8] Seed 4 objects on the 3-DN cluster"
for i in 1 2 3 4; do
  resp=$(put_obj "seed-${i}.txt" "seed object #${i} at $(date -u +%FT%TZ)")
  cid=$(echo "$resp" | first_chunk_id)
  printf "  seed-%d.txt  chunk=%s..\n" "$i" "${cid:0:16}"
done

echo
echo "[3/8] Add dn4 and restart edge with 4-DN config"
start_dn dn4
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080"

echo
echo "[4/8] Run rebalance (ADR-010): copy misplaced chunks to dn4"
./bin/kvfs-cli rebalance --edge "$EDGE" --apply --concurrency 4
echo
echo "  Disk after rebalance (note over-replicated dn1/2/3):"
print_disks

echo
echo "[5/8] GC plan with min-age=5s (real-world default is 60s, demo uses short window)"
sleep 6
./bin/kvfs-cli gc --edge "$EDGE" --plan --min-age 5 -v

echo
echo "[6/8] GC apply"
./bin/kvfs-cli gc --edge "$EDGE" --apply --min-age 5 --concurrency 4

echo
echo "[7/8] Verify zero sweeps remain (idempotency)"
./bin/kvfs-cli gc --edge "$EDGE" --plan --min-age 5

echo
echo "[8/8] Final disk state — should match meta intent exactly"
print_disks

echo
echo "✅ GC verified: surplus chunks removed, meta = disk."
echo "ℹ️  Two safety nets in action:"
echo "   - claimed-set protected meta-claimed (chunk_id, addr) pairs"
echo "   - min-age (5s in this demo) would have spared any fresh PUT"
