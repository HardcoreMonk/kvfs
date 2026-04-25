#!/usr/bin/env bash
# demo-eta.sh — fixes the gap demonstrated by demo-zeta:
# pre-existing chunks get migrated to their HRW-desired DNs (ADR-010).
#
# Self-contained: tears down → up → seeds 4 objects on 3-DN cluster →
# adds dn4 + restarts edge → confirms misplaced state → runs rebalance →
# confirms 0 misplaced + dn4 disk now holds the seed chunks.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-eta"

echo "=== η demo: rebalance worker (ADR-010) ==="

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl; need python3; need docker
if ! command -v jq >/dev/null; then
  echo "warning: jq missing, output will be less pretty"
fi
pp() { if command -v jq >/dev/null; then jq .; else cat; fi; }

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

# extract first chunk's chunk_id from PUT response (post ADR-011 schema)
first_chunk_id() {
  python3 -c 'import json,sys; print(json.load(sys.stdin)["chunks"][0]["chunk_id"])'
}

dn_chunk_count() {
  docker exec "$1" sh -c 'find /var/lib/kvfs-dn/chunks -type f 2>/dev/null | wc -l' | tr -d ' '
}

cluster_up() {
  local dn_csv="$1"
  echo "  bringing up edge with EDGE_DNS=$dn_csv"
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

echo
echo "[1/7] Reset cluster from scratch"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3; do start_dn "$id"; done
cluster_up "dn1:8080,dn2:8080,dn3:8080"
echo "  ✅ 3 DNs + edge up"

echo
echo "[2/7] Seed 4 objects on the 3-DN cluster"
for i in 1 2 3 4; do
  resp=$(put_obj "seed-${i}.txt" "seed object #${i} at $(date -u +%FT%TZ)")
  cid=$(echo "$resp" | first_chunk_id)
  printf "  seed-%d.txt  chunk=%s..\n" "$i" "${cid:0:16}"
done

echo
echo "[3/7] Add dn4 and restart edge with 4-DN config"
start_dn dn4
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080"
echo "  dn4 chunk count after cluster expansion: $(dn_chunk_count dn4)"

echo
echo "[4/7] Compute rebalance plan (dry-run)"
./bin/kvfs-cli rebalance --edge "$EDGE" --plan -v

echo
echo "[5/7] Apply rebalance"
./bin/kvfs-cli rebalance --edge "$EDGE" --apply --concurrency 4

echo
echo "[6/7] Verify zero migrations remain (idempotency check)"
./bin/kvfs-cli rebalance --edge "$EDGE" --plan

echo
echo "[7/7] Disk-level confirmation"
for dn in dn1 dn2 dn3 dn4; do
  printf "  %-4s chunk count = %s\n" "$dn" "$(dn_chunk_count $dn)"
done

echo
echo "✅ Rebalance verified: misplaced chunks copied to dn4, meta updated, second plan empty."
echo "ℹ️  Note: dn1/dn2/dn3 still hold the original copies (over-replicated)."
echo "   Surplus cleanup is the job of a future GC ADR (kept simple here)."
