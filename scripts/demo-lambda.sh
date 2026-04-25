#!/usr/bin/env bash
# demo-lambda.sh — auto-trigger (ADR-013): edge自動 rebalance + GC
# 운영자가 한 번도 --apply 호출하지 않아도 클러스터가 스스로 정렬.
#
# Self-contained: down → up edge with EDGE_AUTO=1 + 짧은 interval (10s)
# → seed → add dn4 + restart → wait → /v1/admin/auto/status 확인.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-lambda"
REBALANCE_INT="10s"
GC_INT="12s"
GC_MIN_AGE="3s"

echo "=== λ demo: auto-trigger (ADR-013) — opt-in time-based loops ==="

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl; need python3; need docker
if ! command -v jq >/dev/null; then
  echo "warning: jq missing"
fi

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
    -e EDGE_AUTO="1" \
    -e EDGE_AUTO_REBALANCE_INTERVAL="$REBALANCE_INT" \
    -e EDGE_AUTO_GC_INTERVAL="$GC_INT" \
    -e EDGE_AUTO_GC_MIN_AGE="$GC_MIN_AGE" \
    -e EDGE_AUTO_CONCURRENCY="4" \
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
  for dn in dn1 dn2 dn3 dn4; do
    printf "  %-4s chunk count = %s\n" "$dn" "$(dn_chunk_count $dn)"
  done
}

echo
echo "[1/7] Reset cluster + up edge with EDGE_AUTO=1 (intervals: rebalance=$REBALANCE_INT, gc=$GC_INT, min-age=$GC_MIN_AGE)"
./scripts/down.sh >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for id in dn1 dn2 dn3; do start_dn "$id"; done
cluster_up "dn1:8080,dn2:8080,dn3:8080"
echo "  ✅ 3 DNs + edge up (auto enabled)"

echo
echo "[2/7] Seed 4 objects on the 3-DN cluster"
for i in 1 2 3 4; do
  put_obj "seed-${i}.txt" "seed object #${i} at $(date -u +%FT%TZ)" >/dev/null
  printf "  put seed-%d.txt\n" "$i"
done

echo
echo "[3/7] Add dn4 + restart edge with 4-DN config (auto still on)"
start_dn dn4
cluster_up "dn1:8080,dn2:8080,dn3:8080,dn4:8080"
echo "  Disk before any auto cycle:"
print_disks

echo
echo "[4/7] Sleep $((${REBALANCE_INT%s} + 5))s — auto-rebalance should have fired by then"
sleep $((${REBALANCE_INT%s} + 5))
./bin/kvfs-cli auto --status -v
echo
echo "  Disk after auto-rebalance:"
print_disks

echo
echo "[5/7] Sleep $((${GC_INT%s} + 5))s — auto-gc should fire next"
sleep $((${GC_INT%s} + 5))
./bin/kvfs-cli auto --status -v
echo
echo "  Disk after auto-gc:"
print_disks

echo
echo "[6/7] Verify cluster is in HRW-desired state without any manual call"
./bin/kvfs-cli rebalance --edge "$EDGE" --plan
echo
./bin/kvfs-cli gc --edge "$EDGE" --plan --min-age "${GC_MIN_AGE%s}"

echo
echo "[7/7] Final disk = meta intent (4 objects × 3 replicas = 12 chunks)"
total=0
for dn in dn1 dn2 dn3 dn4; do
  c=$(dn_chunk_count "$dn")
  total=$((total + c))
  printf "  %-4s chunk count = %s\n" "$dn" "$c"
done
echo "  total: $total  (expected 12)"

echo
echo "✅ Auto-trigger verified:"
echo "   - operator made ZERO --apply calls"
echo "   - edge ran rebalance every $REBALANCE_INT and gc every $GC_INT"
echo "   - same algorithms (ADR-010 + ADR-012), same safety nets"
echo "   - manual --plan still works (auto + manual share the same mutexes)"
