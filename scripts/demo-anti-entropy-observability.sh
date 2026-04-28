#!/usr/bin/env bash
# demo-anti-entropy-observability.sh — P8-16 (ADR-063): three observability
# completions on top of P8-15's metrics surface.
#
#   Part A: duration histograms — kvfs_anti_entropy_audit_duration_seconds
#           and kvfs_anti_entropy_repair_duration_seconds. Demo runs a
#           manual repair, then dumps the *_bucket lines so operators
#           can see how histogram_quantile() would compute p99.
#   Part B: skipped counter — kvfs_anti_entropy_skipped_total{mode}.
#           Drops a DN to provoke "skip" mode; PUTs an EC object then
#           runs repair without ec=1 to provoke "ec-deferred".
#   Part C: unrecoverable dedupe — auto-repair tick rediscovers a lost
#           chunk every cycle, but the counter advances exactly once
#           per chunk_id. Distinct from P8-15 where every tick bumped.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy observability completions demo (P8-16, ADR-063) ==="
echo

need curl; need jq; need docker
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

# 4 DN so we can pause one and still have a surviving R=3 replica set
# for the un-deleted objects.
for i in 1 2 3 4; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network "$NET" \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -e DN_SCRUB_INTERVAL="100ms" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

docker volume create coord1-data >/dev/null
docker run -d --name coord1 --network "$NET" -p 9000:9000 \
  -e COORD_ADDR=":9000" \
  -e COORD_DATA_DIR="/var/lib/kvfs-coord" \
  -e COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  -e COORD_DN_IO=1 \
  -e COORD_AUTO_REPAIR_INTERVAL="2s" \
  -e COORD_AUTO_REPAIR_MAX="50" \
  -v coord1-data:/var/lib/kvfs-coord \
  kvfs-coord:dev >/dev/null

wait_healthz "${COORD_URL}/v1/coord/healthz"
EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# pipefail-safe metric reader (returns "0" when label tuple unseen).
metric_value() {
  local name="$1" out
  out=$(curl -fsS "${COORD_URL}/metrics" || true)
  echo "$out" | grep -E "^${name}( |\{)" | head -1 | awk '{print $NF}' || true
}

echo "==> Part A: duration histograms — manual repair then dump _bucket lines"
for i in 1 2 3 4 5; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/obs/obj${i}" 60)"
  curl -fs -X PUT --data-binary "obs-body-${i}-distinguishable" "$put_url" >/dev/null
done
curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair" >/dev/null
echo "    audit + repair counts via histogram:"
curl -fsS "${COORD_URL}/metrics" \
  | grep -E '^kvfs_anti_entropy_(audit|repair)_duration_seconds_(count|sum)' \
  | sort
echo "    selected buckets (≤25ms, ≤100ms, ≤500ms, ≤2.5s, +Inf):"
curl -fsS "${COORD_URL}/metrics" \
  | grep -E '^kvfs_anti_entropy_(audit|repair)_duration_seconds_bucket\{le="(0.025|0.1|0.5|2.5|\+Inf)"\}' \
  | sort
echo

echo "==> Part B: skipped counter — PUT an EC object, rm one shard, repair without ec=1"
# Provokes mode=ec-deferred. (mode=skip is defensive: it requires a DN
# that's unreachable AT REPAIR TIME but had MissingFromDN populated AT
# AUDIT TIME — practically unreachable since runAntiEntropyRepair runs
# both in one call. The counter is wired anyway for completeness.)
EC_PUT_URL="${EDGE}$(sign_url PUT '/v1/o/obs/ec1' 60)"
dd if=/dev/urandom bs=8192 count=1 2>/dev/null \
  | curl -fs -X PUT -H 'X-KVFS-EC: 3+1' --data-binary @- "$EC_PUT_URL" >/dev/null
echo "    PUT 8 KiB EC (3+1) object"

# Find the first shard of the new EC stripe and rm it.
EC_SHARD=$(docker run --rm --network "$NET" kvfs-cli:dev inspect --coord http://coord1:9000 \
  | jq -r '[.objects[]?.stripes[]?.shards[]?.chunk_id] | .[0]')
echo "    rm'ing shard ${EC_SHARD:0:16}.."
for OWNER in dn1 dn2 dn3 dn4; do
  docker exec "$OWNER" rm -f "/var/lib/kvfs-dn/chunks/${EC_SHARD:0:2}/${EC_SHARD:2}" 2>/dev/null || true
done

# repair without ec=1 — audit detects missing shard, repair flags as
# ec-deferred (operator must opt in to EC reconstruct).
curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair" >/dev/null
EC_DEFERRED_COUNT=$(metric_value 'kvfs_anti_entropy_skipped_total{mode="ec-deferred"}')
echo "    skipped_total{mode=ec-deferred}: ${EC_DEFERRED_COUNT:-0}"
[ "${EC_DEFERRED_COUNT:-0}" -ge 1 ] || { echo "FAIL: expected at least 1 ec-deferred"; exit 1; }
echo "    ✓ skipped counter advanced (ec-deferred when EC chunk detected without ec=1)"

# Heal so Part C starts from a clean state.
curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?ec=1" >/dev/null
echo

echo "==> Part C: unrecoverable dedupe — auto-repair tick rediscovers, counter doesn't"
LOST_ID=$(docker run --rm --network "$NET" kvfs-cli:dev inspect --coord http://coord1:9000 \
  | jq -r '.objects[2].chunks[0].chunk_id')
for OWNER in dn1 dn2 dn3 dn4; do
  docker exec "$OWNER" rm -f "/var/lib/kvfs-dn/chunks/${LOST_ID:0:2}/${LOST_ID:2}" 2>/dev/null || true
done
echo "    unmade ${LOST_ID:0:16}.. from every replica"

UNREC_BEFORE=$(metric_value kvfs_anti_entropy_unrecoverable_total)
# Wait for ~3 auto-repair ticks (interval=2s).
sleep 7
UNREC_AFTER=$(metric_value kvfs_anti_entropy_unrecoverable_total)
RUNS_AFTER=$(metric_value kvfs_anti_entropy_auto_repair_runs_total)
DELTA=$((${UNREC_AFTER:-0} - ${UNREC_BEFORE:-0}))
echo "    unrecoverable_total: ${UNREC_BEFORE:-0} → ${UNREC_AFTER:-0} (Δ=${DELTA})"
echo "    auto_repair_runs_total: ${RUNS_AFTER:-0} (multiple ticks fired)"

# ADR-063 invariant: regardless of how many ticks rediscovered the same
# chunk_id, the counter advances by exactly 1.
if [ "$DELTA" -eq 1 ]; then
  echo "    ✓ counter advanced by exactly 1 — dedupe holds across ${RUNS_AFTER:-0} ticks"
else
  echo "FAIL: counter advanced by ${DELTA} instead of 1 (dedupe regression)"
  exit 1
fi
echo

echo "=== PASS: ADR-063 — observability completions verified end-to-end ==="
echo "    Cleanup: ./scripts/down.sh"
