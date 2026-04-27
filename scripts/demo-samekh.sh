#!/usr/bin/env bash
# demo-samekh.sh — Season 7 Ep.1: failure domain hierarchy (ADR-051).
#
# Setup: 6 DN distributed across 3 failure domains (rack1/2/3, 2 DN per
# rack) + coord + edge (proxy mode). PUT a replicated object with R=3
# and assert that the chosen replicas span all 3 domains — proving the
# topology-aware HRW spreads writes across racks instead of clustering
# them in one. Then PUT an EC (4+2) object and assert that all 6 shards
# of every stripe land on distinct domains too (one shard per rack × 2
# racks, ideally; 6 nodes / 3 domains = at most 2 shards per domain).
#
# This is the "rack-awareness" property that lets you survive a rack-
# level outage even when individual DN failures haven't yet exhausted
# replication.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD_PORT=9000

echo "=== ס samekh demo: failure domain hierarchy (Season 7 Ep.1, ADR-051) ==="
echo "    Hebrew letter 15 — frame 2 (textbook primitive) wave begins"
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

# 6 DN across 3 racks: dn1+dn4 on rack1, dn2+dn5 on rack2, dn3+dn6 on rack3.
echo "==> starting 6 DN"
start_dns 6

# Coord with all 6 DN registered, DN-IO on so PlaceN paths route through
# the same Coordinator instance (single source of truth, P7-07).
echo "==> starting coord (knows dn1..dn6, COORD_DN_IO=1)"
COORD_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  COORD_DN_IO=1 \
  start_coord coord1 $COORD_PORT
wait_healthz "http://localhost:${COORD_PORT}/v1/coord/healthz"

echo "==> tagging failure domains via cli"
for spec in "dn1:8080:rack1" "dn2:8080:rack2" "dn3:8080:rack3" \
            "dn4:8080:rack1" "dn5:8080:rack2" "dn6:8080:rack3"; do
  IFS=: read -r host port rack <<<"$spec"
  docker run --rm --network "$NET" kvfs-cli:dev \
    dns --coord http://coord1:9000 domain "${host}:${port}" "$rack" | tail -1
done
echo

EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"
echo

# ---------------------------------------------------------------
# Probe 1: replicated PUT with R=3 → expect distinct rack per replica
# ---------------------------------------------------------------
echo "==> PUT replicated object (R=3 default) — expect 3 distinct racks"
put_url="${EDGE}$(sign_url PUT /v1/o/samekh/repl 60)"
META=$(curl -fs -X PUT --data-binary 'topology-aware write' "$put_url")
echo "$META" | jq -c '{key: .key, chunks: (.chunks|length), replicas: .chunks[0].replicas}'

# Extract replicas, look up each DN's domain via coord admin, count
# distinct domains. Anything <3 means the diversity walk failed.
REPLICAS=$(echo "$META" | jq -r '.chunks[0].replicas[]')
RACKS=()
for r in $REPLICAS; do
  D=$(curl -fs "http://localhost:9000/v1/coord/admin/dns?detailed=1" | jq -r ".[] | select(.addr==\"${r}\") | .domain")
  RACKS+=("$D")
done
DISTINCT=$(printf '%s\n' "${RACKS[@]}" | sort -u | wc -l | tr -d ' ')
echo "    replica racks: ${RACKS[*]}"
echo "    distinct racks: ${DISTINCT}"
[ "$DISTINCT" -eq 3 ] || { echo "FAIL: expected 3 distinct racks, got ${DISTINCT}"; exit 1; }
echo "    ✓ all 3 replicas in distinct racks"
echo

# ---------------------------------------------------------------
# Probe 2: EC (4+2) — 6 shards per stripe, 6 DN total, 3 racks
# Best case is 2 shards per rack; PickByDomain achieves that since
# n=6 and we have 6 distinct nodes → first 3 picks span all 3 racks,
# fallback path fills slots 4-6 by score allowing same-rack reuse.
# ---------------------------------------------------------------
echo "==> PUT EC (4+2) object — expect each stripe spans all 3 racks"
head -c $((1024 * 1024)) /dev/urandom > /tmp/samekh-ec.bin
put_url="${EDGE}$(sign_url PUT /v1/o/samekh/ec 60)"
ECMETA=$(curl -fs -X PUT -H "X-KVFS-EC: 4+2" --data-binary @/tmp/samekh-ec.bin "$put_url")
NSTRIPES=$(echo "$ECMETA" | jq '.stripes|length')
echo "    stripes: ${NSTRIPES}"

ALL_OK=1
for i in $(seq 0 $((NSTRIPES-1))); do
  SHARDS=$(echo "$ECMETA" | jq -r ".stripes[$i].shards[].replicas[0]")
  RACKS=()
  for s in $SHARDS; do
    D=$(curl -fs "http://localhost:9000/v1/coord/admin/dns?detailed=1" | jq -r ".[] | select(.addr==\"${s}\") | .domain")
    RACKS+=("$D")
  done
  DISTINCT=$(printf '%s\n' "${RACKS[@]}" | sort -u | wc -l | tr -d ' ')
  if [ "$DISTINCT" -ne 3 ]; then
    echo "    stripe ${i}: only ${DISTINCT} distinct racks (${RACKS[*]})"
    ALL_OK=0
  fi
done
[ "$ALL_OK" -eq 1 ] || { echo "FAIL: at least one stripe did not span all 3 racks"; exit 1; }
echo "    ✓ every EC stripe (${NSTRIPES}) spans all 3 racks"
rm -f /tmp/samekh-ec.bin
echo

echo "=== ס PASS: topology-aware HRW spreads replicas + shards across failure domains ==="
echo "    Cleanup: ./scripts/down.sh"
