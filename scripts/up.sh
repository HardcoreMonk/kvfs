#!/usr/bin/env bash
# up.sh — bring up kvfs cluster without docker compose plugin.
# Uses plain `docker build` + `docker run`. Equivalent to `docker compose up -d --build`.
set -euo pipefail

cd "$(dirname "$0")/.."

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"

echo "=== Building images ==="
docker build -t kvfs-dn:dev    --target kvfs-dn   .
docker build -t kvfs-edge:dev  --target kvfs-edge .
docker build -t kvfs-cli:dev   --target kvfs-cli  .

echo
echo "=== Network ==="
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET"

# Volumes
for v in dn1-data dn2-data dn3-data edge-data; do
  docker volume inspect "$v" >/dev/null 2>&1 || docker volume create "$v" >/dev/null
done

echo
echo "=== DataNodes ==="
for i in 1 2 3; do
  name="dn$i"
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" \
    --network "$NET" \
    -e DN_ID="$name" \
    -e DN_ADDR=":8080" \
    -e DN_DATA_DIR="/var/lib/kvfs-dn" \
    -v "$name-data:/var/lib/kvfs-dn" \
    kvfs-dn:dev
  echo "  started $name"
done

echo
echo "=== Edge ==="
docker rm -f edge >/dev/null 2>&1 || true
docker run -d --name edge \
  --network "$NET" \
  -p 8000:8000 \
  -e EDGE_ADDR=":8000" \
  -e EDGE_DNS="dn1:8080,dn2:8080,dn3:8080" \
  -e EDGE_DATA_DIR="/var/lib/kvfs-edge" \
  -e EDGE_URLKEY_SECRET="$SECRET" \
  -e EDGE_QUORUM_WRITE="2" \
  -v "edge-data:/var/lib/kvfs-edge" \
  kvfs-edge:dev
echo "  started edge (http://localhost:8000)"

echo
echo "=== Waiting for edge to be healthy ==="
for i in $(seq 1 30); do
  if curl -fsS http://localhost:8000/healthz >/dev/null 2>&1; then
    echo "  ✅ edge healthy"
    break
  fi
  sleep 0.5
done

echo
echo "Cluster is up. Try:"
echo "  ./scripts/demo-alpha.sh"
echo "  ./scripts/demo-epsilon.sh"
echo "  ./scripts/down.sh     # teardown"
