#!/usr/bin/env bash
# demo-alpha.sh — demonstrates 3-way replication durability.
#
# Prerequisite: ./scripts/up.sh  (cluster must be running)
set -euo pipefail

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo"
KEY="hello.txt"
BODY="Hello from kvfs — 3-way replication demo at $(date -u +%FT%TZ)"

echo "=== α demo: 3-way replication durability ==="

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

path="/v1/o/${BUCKET}/${KEY}"

echo
echo "[1/4] Signing PUT URL..."
put_url="${EDGE}$(sign_url PUT "$path" 3600)"
echo "     → $put_url"

echo
echo "[2/4] PUT object..."
resp=$(curl -fsS -X PUT "$put_url" \
  -H "Content-Type: text/plain" \
  --data-binary "$BODY")
echo "$resp" | pp
chunk_id=$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["chunk_id"])')
echo "     chunk_id=$chunk_id"

echo
echo "[3/4] Verify all 3 DNs have the chunk on disk..."
for dn in dn1 dn2 dn3; do
  # bash 4+: ${var:0:2} / ${var:2}
  prefix="${chunk_id:0:2}"
  rest="${chunk_id:2}"
  if docker exec "$dn" ls "/var/lib/kvfs-dn/chunks/${prefix}/${rest}" >/dev/null 2>&1; then
    echo "     ✅ $dn has ${prefix}/${rest:0:16}..."
  else
    echo "     ❌ $dn missing ${prefix}/${rest}"
    exit 1
  fi
done

echo
echo "[4/4] Kill dn1, GET should still succeed..."
docker stop dn1 >/dev/null
sleep 1
get_url="${EDGE}$(sign_url GET "$path" 3600)"
body=$(curl -fsS "$get_url")
if [ "$body" = "$BODY" ]; then
  echo "     ✅ GET after dn1 kill succeeded, body matches"
else
  echo "     ❌ body mismatch:"
  echo "        expected: $BODY"
  echo "        got:      $body"
  docker start dn1 >/dev/null
  exit 1
fi

# restore dn1
docker start dn1 >/dev/null

echo
echo "✅ 3-way replication verified: object survived 1 DN failure"
