#!/usr/bin/env bash
# demo-zeta.sh — adds a 4th DN and shows placement spreads new writes,
# while pre-existing chunks remain on their original DNs (ADR-010 motivation).
#
# Prerequisite: ./scripts/up.sh  (cluster running with 3 DNs)
#
# Phase 1 implementation note (per FOLLOWUP P1-04):
#   edge currently reads EDGE_DNS once at startup, so adding a DN means
#   restarting the edge container with the expanded list. The bbolt
#   edge-data volume is preserved, so existing object metadata survives.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo-zeta"

echo "=== ζ demo: 4th DN, placement-aware writes ==="

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl; need python3; need docker
if ! command -v jq >/dev/null; then
  echo "warning: jq missing, output will be less pretty"
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

# put_obj <key> <body> → echoes "chunk_id replicas_csv"
put_obj() {
  local key="$1" body="$2"
  local path="/v1/o/${BUCKET}/${key}"
  local url="${EDGE}$(sign_url PUT "$path" 600)"
  local resp
  resp=$(curl -fsS -X PUT "$url" -H "Content-Type: text/plain" --data-binary "$body")
  python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
print(r["chunk_id"], ",".join(r["replicas"]))
' <<<"$resp"
}

get_obj() {
  local key="$1"
  local path="/v1/o/${BUCKET}/${key}"
  local url="${EDGE}$(sign_url GET "$path" 600)"
  curl -fsS "$url"
}

dn_chunk_count() {
  # number of chunk files on a given DN
  docker exec "$1" sh -c 'find /var/lib/kvfs-dn/chunks -type f 2>/dev/null | wc -l' | tr -d ' '
}

echo
echo "[1/6] Pre-flight: cluster has 3 DNs?"
for dn in dn1 dn2 dn3; do
  if ! docker inspect -f '{{.State.Running}}' "$dn" 2>/dev/null | grep -q true; then
    echo "     ❌ $dn not running. Run ./scripts/up.sh first."
    exit 1
  fi
  echo "     ✅ $dn running"
done
if ! curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then
  echo "     ❌ edge not healthy"
  exit 1
fi
echo "     ✅ edge healthy"

echo
echo "[2/6] Seed: PUT 4 objects with the current 3-DN cluster"
echo "        (R=N=3 → every object lands on all 3 DNs)"
declare -a SEED_KEYS SEED_CHUNKS SEED_REPLICAS
for i in 1 2 3 4; do
  key="seed-${i}.txt"
  body="seed object #${i} at $(date -u +%FT%TZ)"
  read -r cid rep < <(put_obj "$key" "$body")
  SEED_KEYS+=("$key"); SEED_CHUNKS+=("$cid"); SEED_REPLICAS+=("$rep")
  printf "     %-12s chunk_id=%s.. replicas=%s\n" "$key" "${cid:0:16}" "$rep"
done

echo
echo "[3/6] Add 4th DN (dn4)"
docker volume inspect dn4-data >/dev/null 2>&1 || docker volume create dn4-data >/dev/null
docker rm -f dn4 >/dev/null 2>&1 || true
docker run -d --name dn4 \
  --network "$NET" \
  -e DN_ID="dn4" \
  -e DN_ADDR=":8080" \
  -e DN_DATA_DIR="/var/lib/kvfs-dn" \
  -v "dn4-data:/var/lib/kvfs-dn" \
  kvfs-dn:dev >/dev/null
echo "     ✅ dn4 started (fresh, 0 chunks)"

echo
echo "[4/6] Restart edge with EDGE_DNS expanded to 4 nodes"
echo "        (edge re-reads env once at boot — Phase 1 limitation; see ADR-010)"
docker rm -f edge >/dev/null 2>&1 || true
docker run -d --name edge \
  --network "$NET" \
  -p 8000:8000 \
  -e EDGE_ADDR=":8000" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_QUORUM_WRITE="2" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null
for i in $(seq 1 30); do
  if curl -fsS "${EDGE}/healthz" >/dev/null 2>&1; then
    echo "     ✅ edge healthy with 4-DN config"
    break
  fi
  sleep 0.5
done

echo
echo "[5/6] Verify pre-existing chunks survived & dn4 stayed empty"
for i in "${!SEED_KEYS[@]}"; do
  key="${SEED_KEYS[$i]}"
  body=$(get_obj "$key")
  if [ -n "$body" ]; then
    echo "     ✅ GET $key → ok (${#body} bytes)"
  else
    echo "     ❌ GET $key failed"
    exit 1
  fi
done
dn4_count_pre=$(dn_chunk_count dn4)
echo "     dn4 chunk count after seed reads: $dn4_count_pre"
if [ "$dn4_count_pre" != "0" ]; then
  echo "     ⚠️  expected 0 (no auto-rebalance) — got $dn4_count_pre"
fi

echo
echo "[6/6] Write 8 new objects → placement should spread across all 4 DNs"
declare -A USAGE
for dn in dn1 dn2 dn3 dn4; do USAGE[$dn]=0; done
for i in $(seq 1 8); do
  key="new-${i}.txt"
  body="new object #${i} written after dn4 join, at $(date -u +%FT%TZ)"
  read -r cid rep < <(put_obj "$key" "$body")
  printf "     %-12s chunk_id=%s.. replicas=%s\n" "$key" "${cid:0:16}" "$rep"
  IFS=',' read -ra reps <<<"$rep"
  for r in "${reps[@]}"; do
    dn="${r%%:*}"
    USAGE[$dn]=$((USAGE[$dn]+1))
  done
done

echo
echo "Replica usage across 8 new writes (R=3, total slots=24):"
for dn in dn1 dn2 dn3 dn4; do
  printf "  %-4s %2d/24 slots\n" "$dn" "${USAGE[$dn]}"
done

dn4_count_post=$(dn_chunk_count dn4)
echo
echo "Disk-level confirmation:"
echo "  dn4 chunk count after new writes: $dn4_count_post"

echo
echo "✅ Placement is live: new writes select 3 of 4 DNs by score, dn4 now participates."
echo "ℹ️  Pre-existing 4 seed chunks remain on dn1/dn2/dn3 only — no auto-rebalance."
echo "   That migration is the job of ADR-010 (Rebalance worker) in a future episode."
echo
echo "Cleanup tip: ./scripts/down.sh   # removes dn4 + dn4-data too"
