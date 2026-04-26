#!/usr/bin/env bash
# up-tls.sh — like up.sh but with full TLS + mTLS (ADR-029).
# Edge HTTPS on :8443, edge ↔ DN mTLS via shared CA.
#
# Prereq: ./scripts/gen-tls-certs.sh
set -euo pipefail

cd "$(dirname "$0")/.."

if [ ! -f certs/ca.crt ]; then
  echo "missing certs/ — run ./scripts/gen-tls-certs.sh first"; exit 2
fi

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"

echo "=== TLS cluster up (ADR-029) ==="
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

# DataNodes — TLS server + verify edge client cert via shared CA
for i in 1 2 3; do
  docker volume create dn${i}-data >/dev/null 2>&1 || true
  docker rm -f dn${i} >/dev/null 2>&1 || true
  docker run -d --name dn${i} \
    --network "$NET" \
    -e DN_ID=dn${i} \
    -e DN_ADDR=:8080 \
    -e DN_DATA_DIR=/var/lib/kvfs-dn \
    -e DN_TLS_CERT=/etc/kvfs/certs/dn.crt \
    -e DN_TLS_KEY=/etc/kvfs/certs/dn.key \
    -e DN_TLS_CLIENT_CA=/etc/kvfs/certs/ca.crt \
    -v dn${i}-data:/var/lib/kvfs-dn \
    -v "$PWD/certs:/etc/kvfs/certs:ro" \
    kvfs-dn:dev >/dev/null
  echo "  ✓ dn${i} HTTPS+mTLS"
done

# Edge — HTTPS server on :8443 + HTTPS client + mTLS cert to DNs
docker rm -f edge >/dev/null 2>&1 || true
docker run -d --name edge \
  --network "$NET" \
  -p 8443:8443 \
  -e EDGE_ADDR=:8443 \
  -e EDGE_DNS=dn1:8080,dn2:8080,dn3:8080 \
  -e EDGE_DATA_DIR=/var/lib/kvfs-edge \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_TLS_CERT=/etc/kvfs/certs/edge.crt \
  -e EDGE_TLS_KEY=/etc/kvfs/certs/edge.key \
  -e EDGE_DN_TLS_CA=/etc/kvfs/certs/ca.crt \
  -e EDGE_DN_TLS_CLIENT_CERT=/etc/kvfs/certs/edge-client.crt \
  -e EDGE_DN_TLS_CLIENT_KEY=/etc/kvfs/certs/edge-client.key \
  -v edge-data:/var/lib/kvfs-edge \
  -v "$PWD/certs:/etc/kvfs/certs:ro" \
  kvfs-edge:dev >/dev/null

for _ in $(seq 1 30); do
  if curl -fsS --cacert certs/ca.crt https://localhost:8443/healthz >/dev/null 2>&1; then
    echo "  ✓ edge HTTPS healthy at https://localhost:8443"
    break
  fi
  sleep 0.5
done

echo
echo "Try:"
echo "  curl --cacert certs/ca.crt https://localhost:8443/healthz"
echo "  curl https://localhost:8443/healthz   # ❌ should fail (cert verify)"
