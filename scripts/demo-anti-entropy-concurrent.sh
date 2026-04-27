#!/usr/bin/env bash
# demo-anti-entropy-concurrent.sh — P8-13 (ADR-060): concurrent EC repair.
#
# ADR-059's per-stripe loop was serial. With many stripes that each need
# Reed-Solomon Reconstruct + multi-shard PUT, latency stacks up. ADR-060
# adds a worker pool so independent stripes repair in parallel.
#
# Stages:
#   1. 6-DN cluster + coord + edge.
#   2. PUT 64 MiB EC (4+2) object — chunk_size 4 MiB · K=4 → stripe_size
#      = 16 MiB → 4 stripes. (configured via EDGE_CHUNK_SIZE if needed.)
#   3. Pick one shard from EACH stripe, rm those files (4 missing shards
#      across 4 stripes — exactly what the worker pool can parallelize).
#   4. Time serial repair (concurrency=1). Wallclock = sum of per-stripe.
#   5. Re-corrupt the same way.
#   6. Time concurrent repair (concurrency=4). Wallclock ≈ slowest stripe.
#   7. GET via edge → sha256 intact in both runs.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy concurrent EC repair demo (P8-13, ADR-060) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

start_dns 6
COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD_URL}/v1/coord/healthz"
EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> stage 1: PUT 64 MiB EC (4+2) object — multi-stripe"
head -c $((64 * 1024 * 1024)) /dev/urandom > /tmp/concurrent-source.bin
EXPECTED_SHA=$(sha256sum /tmp/concurrent-source.bin | awk '{print $1}')
put_url="${EDGE}$(sign_url PUT /v1/o/concurrent/big 60)"
curl -fs -X PUT -H "X-KVFS-EC: 4+2" --data-binary @/tmp/concurrent-source.bin "$put_url" >/dev/null
NSTRIPES=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" \
  | jq -r '.[] | select(.bucket=="concurrent" and .key=="big") | .stripes | length')
echo "    stripes: ${NSTRIPES}"
echo "    expected sha256: ${EXPECTED_SHA:0:32}.."
[ "$NSTRIPES" -ge 2 ] || { echo "FAIL: need >= 2 stripes for concurrency demo, got ${NSTRIPES}"; exit 1; }
echo

corrupt_one_per_stripe() {
  local total
  total=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" \
    | jq -r '.[] | select(.bucket=="concurrent" and .key=="big") | .stripes | length')
  for i in $(seq 0 $((total - 1))); do
    local sid sdn sdnname spath
    sid=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" \
      | jq -r --argjson i "$i" '.[] | select(.bucket=="concurrent" and .key=="big") | .stripes[$i].shards[0].chunk_id')
    sdn=$(curl -fs "${COORD_URL}/v1/coord/admin/objects" \
      | jq -r --argjson i "$i" '.[] | select(.bucket=="concurrent" and .key=="big") | .stripes[$i].shards[0].replicas[0]')
    sdnname="${sdn%:*}"
    spath="/var/lib/kvfs-dn/chunks/${sid:0:2}/${sid:2}"
    docker exec "$sdnname" rm -f "$spath" 2>/dev/null || true
  done
}

run_repair() {
  local concurrency="$1"
  local url
  if [ "$concurrency" -gt 1 ]; then
    url="${COORD_URL}/v1/coord/admin/anti-entropy/repair?ec=1&concurrency=${concurrency}"
  else
    url="${COORD_URL}/v1/coord/admin/anti-entropy/repair?ec=1"
  fi
  local start end ms
  start=$(python3 -c 'import time; print(int(time.time()*1000))')
  REP=$(curl -fsS -X POST "$url")
  end=$(python3 -c 'import time; print(int(time.time()*1000))')
  ms=$((end - start))
  local ok
  ok=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.mode=="ec-inline" and .ok)] | length')
  echo "    concurrency=${concurrency} → ec-inline OK=${ok} elapsed=${ms}ms"
  echo "    summary: $(echo "$REP" | jq -c '[(.repairs // [])[] | select(.mode=="ec-summary")] | first')"
  REPAIR_MS=$ms
}

echo "==> stage 2: corrupt 1 shard per stripe (${NSTRIPES} dead shards)"
corrupt_one_per_stripe
echo "    deleted 1 shard per stripe (${NSTRIPES} total)"
echo

echo "==> stage 3: serial repair (concurrency=1)"
run_repair 1
SERIAL_MS=$REPAIR_MS
echo

echo "==> stage 4: re-corrupt same way for fair comparison"
corrupt_one_per_stripe
echo

echo "==> stage 5: concurrent repair (concurrency=4)"
run_repair 4
CONCURRENT_MS=$REPAIR_MS
echo

echo "==> stage 6: GET via edge — sha256 intact"
get_url="${EDGE}$(sign_url GET /v1/o/concurrent/big 60)"
curl -fs "$get_url" -o /tmp/concurrent-got.bin
GOT_SHA=$(sha256sum /tmp/concurrent-got.bin | awk '{print $1}')
[ "$GOT_SHA" = "$EXPECTED_SHA" ] || { echo "FAIL: sha mismatch"; exit 1; }
echo "    ✓ GET returns intact 64 MiB"
rm -f /tmp/concurrent-source.bin /tmp/concurrent-got.bin
echo

echo "==> latency comparison"
echo "    serial    : ${SERIAL_MS}ms"
echo "    concurrent: ${CONCURRENT_MS}ms"
if [ "$CONCURRENT_MS" -lt "$SERIAL_MS" ]; then
  RATIO=$(python3 -c "print(round($SERIAL_MS / max($CONCURRENT_MS, 1), 2))")
  echo "    speedup  : ${RATIO}× (concurrency=4 vs serial)"
fi
echo

echo "=== PASS: ADR-060 concurrent EC repair — same correctness, parallel reconstruct ==="
echo "    Cleanup: ./scripts/down.sh"
