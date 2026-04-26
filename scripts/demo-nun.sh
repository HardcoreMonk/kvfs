#!/usr/bin/env bash
# demo-nun.sh — Season 6 Ep.7: edge urlkey.Signer propagation (ADR-049).
#
# Sequence:
#   1. coord + edge with EDGE_COORD_URLKEY_POLL_INTERVAL=3s.
#   2. Sign URL with the boot kid → 200.
#   3. cli urlkey rotate --coord (new kid v2 primary).
#   4. Wait ~4s for poll → edge sees v2.
#   5. Sign URL with v2 → 200 (proves edge picked up the new kid).
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
COORD="http://localhost:9000"
POLL=3s

echo "=== נ nun demo: edge urlkey.Signer propagation (Season 6 Ep.7, ADR-049) ==="

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true
start_dns 3
start_coord coord1 9000
wait_healthz "${COORD}/v1/coord/healthz"

# Edge with short poll interval for the demo.
docker volume create edge-data >/dev/null
docker run -d --name edge --network "$NET" \
  -p 8000:8000 \
  -e EDGE_ADDR=":8000" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_COORD_URL="http://coord1:9000" \
  -e EDGE_COORD_URLKEY_POLL_INTERVAL="$POLL" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null
wait_healthz "${EDGE}/healthz"

echo "==> edge boot log (look for 'urlkey-sync from coord enabled')"
docker logs edge 2>&1 | grep -E 'urlkey-sync|Ep.7' | head -3 | sed 's/^/    /'
echo

# Step 1: PUT with boot kid (v1, the EDGE_URLKEY_SECRET-backed key).
echo "==> [boot kid] PUT — should 201"
put_url="${EDGE}$(sign_url PUT /v1/o/nun/before 60)"
curl -fs -X PUT --data-binary 'before rotate' "$put_url" \
  | jq -c '{bucket, key, size}'
echo

# Step 2: rotate via coord — new kid v2 (auto-gen secret).
echo "==> rotate via coord → new kid v2 (auto-generated secret)"
ROTATE_RESP=$(docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 rotate --kid v2)
echo "$ROTATE_RESP" | head -3

# Extract the new secret from the rotation response so we can sign with it.
NEW_SECRET=$(echo "$ROTATE_RESP" | grep -oE '32-byte secret' | head -1 || true)

# The cli auto-gen prints the secret to its OUTPUT (it does an HTTP POST that
# echoes back kid+is_primary), but the actual hex isn't echoed. For the demo
# we re-rotate with a known secret so we can sign with it locally.
DEMO_SECRET="cafef00d000000000000000000000000000000000000000000000000beefdead"
echo
echo "==> re-rotate with known secret so we can sign locally"
docker run --rm --network "$NET" kvfs-cli:dev \
  urlkey --coord http://coord1:9000 rotate --kid v3 --secret-hex "$DEMO_SECRET" >/dev/null
echo

echo "==> waiting ${POLL} × 2 for edge poller to pick up the new kid"
sleep 7

echo "==> verify edge log shows the sync"
docker logs edge 2>&1 | tail -10 | grep -i 'urlkey-sync' | head -3 | sed 's/^/    /' || true
echo

# Step 3: sign with the new kid v3 + secret, send through edge → 200 means
# edge's signer knows v3 (i.e. the poll worked).
echo "==> [new kid v3 from coord] PUT — should 201 (proves propagation)"
SECRET="$DEMO_SECRET" SIGNED=$(SECRET="$DEMO_SECRET" python3 -c '
import hmac, hashlib, time, sys, os
secret=os.environ["SECRET"].encode(); method="PUT"; path="/v1/o/nun/after"; ttl=60
exp=int(time.time())+ttl
sig=hmac.new(secret,f"{method}:{path}:{exp}".encode(),hashlib.sha256).hexdigest()
print(f"{path}?sig={sig}&exp={exp}&kid=v3")
')
PUT_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PUT --data-binary 'after rotate' "${EDGE}${SIGNED}")
echo "    PUT status = $PUT_CODE"
if [ "$PUT_CODE" = "201" ] || [ "$PUT_CODE" = "200" ]; then
  echo "    ✓ edge accepted the new-kid signature — propagation worked"
else
  echo "    note: status $PUT_CODE — poll may still be in flight, or v3 didn't reach edge"
  echo "    edge log (last 15 lines):"
  docker logs edge 2>&1 | tail -15 | sed 's/^/      /'
fi

echo
echo "=== נ DEMO: edge picks up coord-side urlkey rotation via polling ==="
echo "    Cleanup: ./scripts/down.sh"
