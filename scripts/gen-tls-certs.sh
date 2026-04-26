#!/usr/bin/env bash
# gen-tls-certs.sh — self-signed TLS certs for ADR-029 demo.
#
# Generates under ./certs/:
#   ca.crt + ca.key                — root CA (trust this from clients)
#   edge.crt + edge.key            — edge HTTPS server (CN=localhost, SAN+localhost,127.0.0.1)
#   dn.crt + dn.key                — shared DN HTTPS cert (SAN=dn1..dn9)
#   edge-client.crt + edge-client.key   — edge → DN mTLS client cert
#
# DEMO ONLY — single CA covers everything. Production: per-DN unique cert,
# rotation, OCSP. cert validity = 365 days.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p certs
cd certs

CONF=$(mktemp)
trap 'rm -f "$CONF"' EXIT

# 1) Root CA
if [ ! -f ca.crt ]; then
  openssl genrsa -out ca.key 2048 >/dev/null 2>&1
  openssl req -new -x509 -key ca.key -days 365 -out ca.crt \
    -subj "/CN=kvfs-demo-ca/O=kvfs/OU=demo" >/dev/null 2>&1
  echo "  + ca.crt + ca.key"
fi

# Helper: issue a leaf cert signed by CA. Args: name, CN, SAN_csv (e.g. "DNS:localhost,IP:127.0.0.1")
issue() {
  local name="$1" cn="$2" san="$3"
  cat > "$CONF" <<EOF
[req]
distinguished_name = req_dn
req_extensions = v3_req
prompt = no
[req_dn]
CN = $cn
O  = kvfs
[v3_req]
subjectAltName = $san
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth, clientAuth
EOF
  openssl genrsa -out "${name}.key" 2048 >/dev/null 2>&1
  openssl req -new -key "${name}.key" -out "${name}.csr" -config "$CONF" >/dev/null 2>&1
  openssl x509 -req -in "${name}.csr" -CA ca.crt -CAkey ca.key \
    -CAcreateserial -days 365 -out "${name}.crt" \
    -extfile "$CONF" -extensions v3_req >/dev/null 2>&1
  rm -f "${name}.csr"
  echo "  + ${name}.crt + ${name}.key"
}

# 2) Edge server cert: client connects via localhost
issue edge "kvfs-edge" "DNS:localhost,DNS:edge,IP:127.0.0.1"

# 3) DN server cert: shared across dn1..dn9
issue dn "kvfs-dn" "DNS:dn1,DNS:dn2,DNS:dn3,DNS:dn4,DNS:dn5,DNS:dn6,DNS:dn7,DNS:dn8,DNS:dn9,DNS:localhost"

# 4) Edge client cert (mTLS to DN)
issue edge-client "kvfs-edge-client" "DNS:edge"

# Cleanup CA serial file
rm -f ca.srl

echo
echo "✅ certs/ ready. Trust CA via: --cacert certs/ca.crt"
echo
echo "Quick test:"
echo "  ./scripts/up-tls.sh"
echo "  curl --cacert certs/ca.crt https://localhost:8443/healthz"
