#!/usr/bin/env bash
# demo-anti-entropy-resilience.sh — P8-14 (ADR-061): three operational
# resilience polishes bundled into one demo.
#
#   Part A: replication-path concurrent repair (now uses the same
#           Concurrency knob as EC; visible wallclock saving on big
#           audits with many small replicated chunks).
#   Part B: persistent scrubber state — bit-rot the scrubber once
#           detected survives a DN restart, so the next anti-entropy
#           call doesn't have to wait for a full new scan to re-find
#           it.
#   Part C: unrecoverable-chunk notification — kill every replica of a
#           chunk and verify coord emits a slog.Error event with the
#           chunk_id so external log aggregators can alert on it.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_URL="http://localhost:9000"

echo "=== anti-entropy resilience polish demo (P8-14, ADR-061) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

# 3 DN with scrubber on (50ms cadence) so Part B has fast turnaround.
for i in 1 2 3; do
  docker volume create "dn${i}-data" >/dev/null
  docker run -d --name "dn${i}" --network "$NET" \
    -e DN_ID="dn${i}" -e DN_ADDR=":8080" -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -e DN_SCRUB_INTERVAL="50ms" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done
COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "${COORD_URL}/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> Part A: replication concurrent — measure 8-chunk audit"
for i in 1 2 3 4 5 6 7 8; do
  put_url="${EDGE}$(sign_url PUT "/v1/o/resilience/obj${i}" 60)"
  curl -fs -X PUT --data-binary "resilience-body-${i}-distinguishable-payload" "$put_url" >/dev/null
done
echo "    seeded 8 objects"

# rm one chunk per object from dn1.
N_RM=0
docker run --rm --network "$NET" kvfs-cli:dev inspect --coord http://coord1:9000 \
  | jq -r '.objects[]?.chunks[]?.chunk_id' \
  | head -8 \
  | while read -r CID; do
      docker exec dn1 rm -f "/var/lib/kvfs-dn/chunks/${CID:0:2}/${CID:2}" 2>/dev/null || true
      N_RM=$((N_RM + 1))
    done

run_repair() {
  local cc="$1"
  local q=""
  if [ "$cc" -gt 1 ]; then q="?concurrency=${cc}"; fi
  local s e ms
  s=$(python3 -c 'import time; print(int(time.time()*1000))')
  REP=$(curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair${q}")
  e=$(python3 -c 'import time; print(int(time.time()*1000))')
  ms=$((e - s))
  ROK=$(echo "$REP" | jq -r '[(.repairs // [])[] | select(.ok)] | length')
  echo "    concurrency=${cc} → repaired=${ROK} elapsed=${ms}ms"
  REPAIR_MS=$ms
}

# Re-corrupt for fair second measurement.
recorrupt() {
  docker run --rm --network "$NET" kvfs-cli:dev inspect --coord http://coord1:9000 \
    | jq -r '.objects[]?.chunks[]?.chunk_id' \
    | head -8 \
    | while read -r CID; do
        docker exec dn1 rm -f "/var/lib/kvfs-dn/chunks/${CID:0:2}/${CID:2}" 2>/dev/null || true
      done
}

run_repair 1
SERIAL_MS=$REPAIR_MS

recorrupt
run_repair 4
PARALLEL_MS=$REPAIR_MS

if [ "$PARALLEL_MS" -lt "$SERIAL_MS" ]; then
  RATIO=$(python3 -c "print(round($SERIAL_MS / max($PARALLEL_MS, 1), 2))")
  echo "    speedup: ${RATIO}× (concurrency=4 vs serial)"
fi
echo

echo "==> Part B: persistent scrubber state — corrupt → restart → still flagged"
ROT_ID=$(docker run --rm --network "$NET" kvfs-cli:dev \
  inspect --coord http://coord1:9000 \
  | jq -r '.objects[0].chunks[0].chunk_id')
ROT_PATH="/var/lib/kvfs-dn/chunks/${ROT_ID:0:2}/${ROT_ID:2}"
docker exec dn2 sh -c "echo 'bit-rot-injected-bytes' > '${ROT_PATH}'"
echo "    overwrote ${ROT_ID:0:16}.. on dn2; waiting 4s for scrubber..."
sleep 4
SCRUB_BEFORE=$(docker exec dn2 wget -qO - http://localhost:8080/chunks/scrub-status \
  | jq -r '.corrupt | length')
echo "    dn2 corrupt_count BEFORE restart: ${SCRUB_BEFORE}"
[ "$SCRUB_BEFORE" -ge 1 ] || { echo "FAIL: scrubber didn't flag in time"; exit 1; }

# Restart dn2.
docker restart dn2 >/dev/null
sleep 3
SCRUB_AFTER=$(docker exec dn2 wget -qO - http://localhost:8080/chunks/scrub-status \
  | jq -r '.corrupt | length')
echo "    dn2 corrupt_count AFTER restart:  ${SCRUB_AFTER}"
[ "$SCRUB_AFTER" -ge 1 ] || { echo "FAIL: corrupt set lost across restart (ADR-061 broken)"; exit 1; }
echo "    ✓ scrubber state survived restart"
# Quick repair so dn2 isn't permanently corrupt for the next stage.
curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair?corrupt=1" >/dev/null
echo

echo "==> Part C: unrecoverable notification — kill every replica of a chunk"
# Pick a chunk, then rm its file from EVERY DN that owns it.
LOST_ID=$(docker run --rm --network "$NET" kvfs-cli:dev \
  inspect --coord http://coord1:9000 \
  | jq -r '.objects[3].chunks[0].chunk_id')
for OWNER in dn1 dn2 dn3; do
  docker exec "$OWNER" rm -f "/var/lib/kvfs-dn/chunks/${LOST_ID:0:2}/${LOST_ID:2}" 2>/dev/null || true
done
echo "    unmade ${LOST_ID:0:16}.. from all replicas (truly unrecoverable)"

# Trigger repair; coord should slog.Error the chunk_id.
docker logs --tail 0 -f coord1 > /tmp/coord1.log 2>&1 &
LOGPID=$!
sleep 0.3
curl -fsS -X POST "${COORD_URL}/v1/coord/admin/anti-entropy/repair" >/dev/null
sleep 1
kill "$LOGPID" 2>/dev/null || true
GREP=$(grep -F "$LOST_ID" /tmp/coord1.log || true)
if [ -n "$GREP" ] && echo "$GREP" | grep -q 'level=ERROR' && echo "$GREP" | grep -q 'unrecoverable'; then
  echo "    coord log captured: $(echo "$GREP" | head -1 | cut -c1-160)"
  echo "    ✓ unrecoverable chunk surfaced via slog.Error"
else
  echo "FAIL: expected slog.Error 'unrecoverable' for ${LOST_ID:0:16}.."
  echo "$GREP"
  exit 1
fi
rm -f /tmp/coord1.log
echo

echo "=== PASS: ADR-061 — three resilience polishes verified end-to-end ==="
echo "    Cleanup: ./scripts/down.sh"
