#!/usr/bin/env bash
# demo-anti-entropy-auto-metrics.sh — P8-15 (ADR-062): two operational
# observability features bundled into one demo.
#
#   Part A: scheduled auto-repair — COORD_AUTO_REPAIR_INTERVAL kicks
#           the repair path on the leader periodically. Demo corrupts a
#           chunk, waits one tick, and confirms the chunk is healed
#           without an explicit operator call.
#   Part B: /metrics counters — kvfs_anti_entropy_audits_total /
#           repairs_total{reason} / unrecoverable_total / throttled_total
#           / auto_repair_runs_total. Hits coord's /metrics endpoint and
#           prints the lines, then verifies they advanced after the
#           auto-repair fired and after we induced an unrecoverable.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy auto-repair + /metrics demo (P8-15, ADR-062) ==="
echo

need curl; need jq; need docker
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

# 3 DN with scrubber on so corrupt detection is fast.
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network "$NET" \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -e DN_SCRUB_INTERVAL="50ms" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

# Coord with auto-repair every 2s + DN_IO + metrics on (default).
docker volume create coord1-data >/dev/null
docker run -d --name coord1 --network "$NET" -p 9000:9000 \
  -e COORD_ADDR=":9000" \
  -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
  -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e COORD_DN_IO=1 \
  -e COORD_AUTO_REPAIR_INTERVAL="2s" \
  -e COORD_AUTO_REPAIR_MAX="50" \
  -e COORD_AUTO_REPAIR_CONCURRENCY="4" \
  -v coord1-data:/var/lib/kvfs-coord \
  kvfs-coord:dev >/dev/null

wait_healthz "${COORD_URL}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# Helper: pull a single metric line. Returns "0" when the label tuple
# hasn't been seen yet (Prometheus omits unincremented label slots — our
# metrics package follows the same convention). pipefail-safe.
metric_value() {
  local name="$1" out
  out=$(curl -fsS "${COORD_URL}/metrics" || true)
  echo "$out" | grep -E "^${name}( |\{)" | head -1 | awk '{print $NF}' || true
}

echo "==> Part A: seed 3 objects, then sleep so auto-repair has nothing to do"
for i in 1 2 3; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/auto/obj${i}" 60)"
  curl -fs -X PUT --data-binary "auto-body-${i}-distinguishable" "$put_url" >/dev/null
done

# Wait for at least one auto-repair tick so the counter starts advancing.
sleep 3
RUNS_BEFORE=$(metric_value kvfs_anti_entropy_auto_repair_runs_total)
echo "    auto-repair runs after 3s wait: ${RUNS_BEFORE:-0} (≥1 expected)"
[ "${RUNS_BEFORE:-0}" -ge 1 ] || { echo "FAIL: auto-repair ticker did not fire"; exit 1; }

# Take a baseline of the repair counter.
REPAIRS_MISSING_BEFORE=$(metric_value 'kvfs_anti_entropy_repairs_total{reason="missing"}')
echo "    repairs{reason=missing} baseline: ${REPAIRS_MISSING_BEFORE:-0}"

echo
echo "==> Part B: rm one chunk on dn1 → wait for next tick → verify metric advanced"
TARGET_ID=$(docker run --rm --network "$NET" kvfs-cli:dev inspect --coord http://coord1:9000 \
  | jq -r '.objects[0].chunks[0].chunk_id')
docker exec dn1 rm -f "/var/lib/kvfs-dn/chunks/${TARGET_ID:0:2}/${TARGET_ID:2}"
echo "    rm'd ${TARGET_ID:0:16}.. on dn1"

# Auto-repair tick is 2s; wait 4s for one full cycle.
sleep 4
REPAIRS_MISSING_AFTER=$(metric_value 'kvfs_anti_entropy_repairs_total{reason="missing"}')
echo "    repairs{reason=missing} after auto-tick: ${REPAIRS_MISSING_AFTER:-0}"
DELTA=$((${REPAIRS_MISSING_AFTER:-0} - ${REPAIRS_MISSING_BEFORE:-0}))
[ "$DELTA" -ge 1 ] || { echo "FAIL: missing-repair counter didn't advance (auto-repair didn't heal)"; exit 1; }
echo "    ✓ counter advanced by ${DELTA} (auto-repair fired and counted)"

echo
echo "==> Part C: kill all replicas of one chunk → verify unrecoverable counter"
LOST_ID=$(docker run --rm --network "$NET" kvfs-cli:dev inspect --coord http://coord1:9000 \
  | jq -r '.objects[1].chunks[0].chunk_id')
for OWNER in dn1 dn2 dn3; do
  docker exec "$OWNER" rm -f "/var/lib/kvfs-dn/chunks/${LOST_ID:0:2}/${LOST_ID:2}" 2>/dev/null || true
done
echo "    unmade ${LOST_ID:0:16}.. from every replica"

UNREC_BEFORE=$(metric_value kvfs_anti_entropy_unrecoverable_total)
sleep 4
UNREC_AFTER=$(metric_value kvfs_anti_entropy_unrecoverable_total)
echo "    unrecoverable_total: ${UNREC_BEFORE:-0} → ${UNREC_AFTER:-0}"
[ "${UNREC_AFTER:-0}" -gt "${UNREC_BEFORE:-0}" ] || { echo "FAIL: unrecoverable counter didn't advance"; exit 1; }
echo "    ✓ unrecoverable counter advanced (slog.Error + metric in lockstep, ADR-061+ADR-062)"

echo
echo "==> /metrics snapshot (anti-entropy slice)"
curl -fsS "${COORD_URL}/metrics" | grep '^kvfs_anti_entropy_' | sort
echo
echo "=== PASS: ADR-062 — auto-repair scheduling + Prometheus surface verified ==="
echo "    Cleanup: ./scripts/down.sh"
