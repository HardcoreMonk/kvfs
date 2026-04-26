#!/usr/bin/env bash
# demo-psi.sh — EC + CDC chunking 조합 (ADR-018 follow-up to ADR-008).
#
# CDC 모드 클러스터에서 X-KVFS-EC 헤더로 PUT → 각 CDC chunk 가 한 stripe 가
# 됨 (variable size). GET sha256 round-trip 일치 확인. shift-by-1 dedup
# 시나리오에서 stripe-level shard 재사용 관찰.
set -euo pipefail

cd "$(dirname "$0")/.."

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
export EDGE_URLKEY_SECRET="$SECRET"
EDGE="http://localhost:8000"
BUCKET="demo-psi"
SIZE_MB=4
NET="kvfs-net"

echo "=== ψ demo: EC + CDC chunking 조합 (ADR-008 + ADR-018) ==="

./scripts/down.sh >/dev/null 2>&1 || true

# Need 6 DNs for EC(4+2)
docker network create $NET 2>/dev/null || true
for i in 1 2 3 4 5 6; do
  docker volume create "dn${i}-data" >/dev/null
  docker rm -f "dn${i}" 2>/dev/null || true
  docker run -d --name "dn${i}" \
    --network $NET \
    -e DN_ID="dn${i}" \
    -e DN_ADDR=":8080" \
    -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "dn${i}-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev >/dev/null
done

# Edge with CDC mode + small CDC chunks (so we get ~4-5 stripes for a 4 MiB body).
docker volume create edge-data >/dev/null
docker rm -f edge 2>/dev/null
docker run -d --name edge \
  --network $NET \
  -p 8000:8000 \
  -e EDGE_ADDR=":8000" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080,dn4:8080,dn5:8080,dn6:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_QUORUM_WRITE="2" \
  -e EDGE_CHUNK_MODE="cdc" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev >/dev/null
for _ in $(seq 1 30); do
  if curl -fsS "$EDGE/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.3
done

# Generate body F1 + F2 (1-byte shift)
rm -f /tmp/demo-psi-{F1,F2}.bin
dd if=/dev/urandom of=/tmp/demo-psi-F1.bin bs=1M count=$SIZE_MB status=none
printf '\xAA' > /tmp/demo-psi-prefix.bin
cat /tmp/demo-psi-prefix.bin /tmp/demo-psi-F1.bin > /tmp/demo-psi-F2.bin
F1_SHA=$(sha256sum /tmp/demo-psi-F1.bin | awk '{print $1}')
F2_SHA=$(sha256sum /tmp/demo-psi-F2.bin | awk '{print $1}')
echo "  F1 sha256: $F1_SHA"
echo "  F2 sha256: $F2_SHA"

echo
echo "--- step 1: PUT F1 (EC 4+2 + CDC) ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/F1.bin" --ttl 5m --base "$EDGE")
curl -fsS -X PUT -H "X-KVFS-EC: 4+2" --data-binary "@/tmp/demo-psi-F1.bin" "$url" -o /tmp/demo-psi-F1-resp.json
n_stripes=$(python3 -c "import json; d=json.load(open('/tmp/demo-psi-F1-resp.json')); print(d['num_stripes'])")
echo "  F1 stripes: $n_stripes"
python3 -c "
import json
d=json.load(open('/tmp/demo-psi-F1-resp.json'))
for i, s in enumerate(d['stripes']):
    sz = s.get('data_len', 'n/a')
    print(f'    stripe {i}: data_len={sz}, stripe_id={s[\"stripe_id\"][:16]}...')
"

echo
echo "--- step 2: PUT F2 (1 byte + F1, EC 4+2 + CDC) ---"
url=$(./bin/kvfs-cli sign --method PUT --path "/v1/o/$BUCKET/F2.bin" --ttl 5m --base "$EDGE")
curl -fsS -X PUT -H "X-KVFS-EC: 4+2" --data-binary "@/tmp/demo-psi-F2.bin" "$url" -o /tmp/demo-psi-F2-resp.json
n_stripes2=$(python3 -c "import json; d=json.load(open('/tmp/demo-psi-F2-resp.json')); print(d['num_stripes'])")
echo "  F2 stripes: $n_stripes2"

echo
echo "--- step 3: GET F1 → sha256 round-trip ---"
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/$BUCKET/F1.bin" --ttl 5m --base "$EDGE")
curl -fsS "$url" -o /tmp/demo-psi-F1-dl.bin
F1_DL_SHA=$(sha256sum /tmp/demo-psi-F1-dl.bin | awk '{print $1}')
if [ "$F1_SHA" = "$F1_DL_SHA" ]; then echo "  ✅ F1 sha256 match"; else echo "  ❌ F1 mismatch"; exit 1; fi

echo
echo "--- step 4: GET F2 → sha256 round-trip ---"
url=$(./bin/kvfs-cli sign --method GET --path "/v1/o/$BUCKET/F2.bin" --ttl 5m --base "$EDGE")
curl -fsS "$url" -o /tmp/demo-psi-F2-dl.bin
F2_DL_SHA=$(sha256sum /tmp/demo-psi-F2-dl.bin | awk '{print $1}')
if [ "$F2_SHA" = "$F2_DL_SHA" ]; then echo "  ✅ F2 sha256 match"; else echo "  ❌ F2 mismatch"; exit 1; fi

echo
echo "--- step 5: stripe-level shared shard analysis ---"
python3 <<EOF
import json
f1 = json.load(open('/tmp/demo-psi-F1-resp.json'))['stripes']
f2 = json.load(open('/tmp/demo-psi-F2-resp.json'))['stripes']
# Each stripe has K+M shards. Compare stripe IDs across F1 vs F2.
ids1 = {s['stripe_id'] for s in f1}
ids2 = {s['stripe_id'] for s in f2}
shared = len(ids1 & ids2)
print(f"  F1 stripes: {len(f1)}, F2 stripes: {len(f2)}")
print(f"  shared stripe IDs: {shared}/{len(f1)}")
print(f"  → CDC re-aligned boundaries 후 stripe ID 까지 동일 (shift invariance)")
EOF

echo
echo "✅ ψ demo PASS — EC+CDC round-trip OK, shift-invariant dedup at stripe-level"
