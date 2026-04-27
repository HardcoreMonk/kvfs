#!/usr/bin/env bash
# demo-ayin.sh — Season 7 Ep.2: degraded read (ADR-052).
#
# Setup: 6 DN + coord + edge + EC (4+2). PUT a 1 MB object → 6 shards
# per stripe. Then kill 2 DN holding shards. GET should still succeed
# via parallel-fetch + first-K-wins (textbook Reed-Solomon "any K"
# property realized at read time, not just at repair time).
#
# What pre-S7 did: serial loop over (K+M) shards, falling back replica-
# by-replica per shard. A single dead DN added a connection-refused
# RTT to the GET path; two dead DNs added two. With parallel fetch the
# GET completes at the speed of the K-th fastest shard — the dead
# fetches are cancelled the moment K survivors arrive.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_PORT=9000

echo "=== ע ayin demo: degraded read (Season 7 Ep.2, ADR-052) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

start_dns 6
COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  COORD_DN_IO=1 start_coord coord1 $COORD_PORT
wait_healthz "http://localhost:${COORD_PORT}/v1/coord/healthz"

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

echo "==> PUT 1 MB object with EC (4+2)"
head -c $((1024 * 1024)) /dev/urandom > /tmp/ayin-source.bin
EXPECTED_SHA=$(sha256sum /tmp/ayin-source.bin | awk '{print $1}')
put_url="${EDGE}$(sign_url PUT /v1/o/ayin/data 60)"
ECMETA=$(curl -fs -X PUT -H "X-KVFS-EC: 4+2" \
  --data-binary @/tmp/ayin-source.bin "$put_url")
echo "    stripes: $(echo "$ECMETA" | jq '.stripes|length')"
echo "    expected sha256: ${EXPECTED_SHA:0:32}.."
echo

echo "==> baseline GET (all 6 DN alive) — measure latency"
get_url="${EDGE}$(sign_url GET /v1/o/ayin/data 60)"
T0=$(date +%s%N)
curl -fs "$get_url" -o /tmp/ayin-baseline.bin
T1=$(date +%s%N)
BASELINE_MS=$(( (T1 - T0) / 1000000 ))
GOT_SHA=$(sha256sum /tmp/ayin-baseline.bin | awk '{print $1}')
[ "$GOT_SHA" = "$EXPECTED_SHA" ] || { echo "FAIL: baseline sha mismatch"; exit 1; }
echo "    baseline elapsed: ${BASELINE_MS} ms, sha verified ✓"
echo

echo "==> kill 2 DN (dn5, dn6) — leaves 4 alive (= K)"
docker stop dn5 dn6 >/dev/null
echo "    alive: dn1, dn2, dn3, dn4 (4 of 6 = K survivors → reconstruct path)"
echo

echo "==> GET with 2/6 DN dead — must succeed via parallel-fetch + RS reconstruct"
get_url="${EDGE}$(sign_url GET /v1/o/ayin/data 60)"
T0=$(date +%s%N)
curl -fs "$get_url" -o /tmp/ayin-degraded.bin
T1=$(date +%s%N)
DEGRADED_MS=$(( (T1 - T0) / 1000000 ))
GOT_SHA=$(sha256sum /tmp/ayin-degraded.bin | awk '{print $1}')
[ "$GOT_SHA" = "$EXPECTED_SHA" ] || { echo "FAIL: degraded sha mismatch"; exit 1; }
echo "    degraded elapsed: ${DEGRADED_MS} ms, sha verified ✓"
echo

echo "==> verify metric counter"
DEGRADED_COUNT=$(curl -fs "${EDGE}/metrics" 2>/dev/null \
  | grep -E '^kvfs_ec_degraded_read_total' | awk '{print $2}' || echo "0")
echo "    kvfs_ec_degraded_read_total = ${DEGRADED_COUNT}"
[ "${DEGRADED_COUNT%.*}" -ge 1 ] 2>/dev/null || \
  echo "    (note: metric not visible if EDGE_METRICS=0 or counter not yet flushed)"
echo

# Recovery probe: serial-fallback world would have GET wait on each
# dead DN's TCP timeout (~kernel default ~3s on Linux for RST). Parallel
# completes at K-th-fastest. Hard to make this deterministic in CI so
# we just report; demo audience can compare BASELINE_MS to DEGRADED_MS.
echo "==> latency comparison"
echo "    baseline (6 alive):  ${BASELINE_MS} ms"
echo "    degraded (4 alive):  ${DEGRADED_MS} ms"
echo "    overhead (RS reconstruct work + concurrent fetch coordination)"
echo "    pre-S7 a serial fetch would have added 2× connect-refused RTTs"
echo "    plus per-shard fallback walks; first-K-wins skips that wait."
echo

# Cleanup mid-state — restart killed DNs so down.sh teardown is clean.
docker start dn5 dn6 >/dev/null 2>&1 || true
rm -f /tmp/ayin-source.bin /tmp/ayin-baseline.bin /tmp/ayin-degraded.bin

echo "=== ע PASS: GET completes via K survivors + RS Reconstruct, sha verified ==="
echo "    Cleanup: ./scripts/down.sh"
