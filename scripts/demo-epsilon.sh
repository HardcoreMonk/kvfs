#!/usr/bin/env bash
# demo-epsilon.sh — demonstrates UrlKey presigned URL: valid succeeds, expired fails.
set -euo pipefail

SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"
BUCKET="demo"
KEY="sign-test.txt"
BODY="signed by UrlKey at $(date -u +%FT%TZ)"

echo "=== ε demo: UrlKey presigned URL ==="

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl

sign_url_py() {
  SECRET=$SECRET python3 -c '
import hmac, hashlib, time, sys, os
secret=os.environ["SECRET"].encode(); method=sys.argv[1]; path=sys.argv[2]; ttl=int(sys.argv[3])
exp=int(time.time())+ttl
sig=hmac.new(secret,f"{method}:{path}:{exp}".encode(),hashlib.sha256).hexdigest()
print(f"{path}?sig={sig}&exp={exp}")
' "$1" "$2" "$3"
}

path="/v1/o/${BUCKET}/${KEY}"

echo
echo "[1/3] Sign a valid PUT URL (TTL 60s)..."
valid=$(sign_url_py PUT "$path" 60)
echo "     → ${EDGE}${valid}"

echo
echo "[2/3] PUT with valid URL → should succeed..."
if curl -fsS -X PUT "${EDGE}${valid}" -H "Content-Type: text/plain" --data-binary "$BODY" >/dev/null; then
  echo "     ✅ valid URL accepted"
else
  echo "     ❌ valid URL rejected"
  exit 1
fi

echo
echo "[3/3] Sign an already-expired URL → should fail with 401..."
expired=$(sign_url_py PUT "$path" -60)  # 60s in the past
code=$(curl -s -o /dev/null -w "%{http_code}" -X PUT "${EDGE}${expired}" -H "Content-Type: text/plain" --data-binary "late")
if [ "$code" = "401" ]; then
  echo "     ✅ expired URL rejected (HTTP 401)"
else
  echo "     ❌ expected 401, got $code"
  exit 1
fi

echo
echo "✅ UrlKey presigned URL verified"
