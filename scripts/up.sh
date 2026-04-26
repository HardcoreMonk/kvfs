#!/usr/bin/env bash
# up.sh — bring up kvfs cluster without docker compose plugin.
# Uses lib/cluster.sh helpers; equivalent to `docker compose up -d --build`.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"

echo "=== Building images ==="
docker build -t kvfs-dn:dev   --target kvfs-dn   . >/dev/null
docker build -t kvfs-edge:dev --target kvfs-edge . >/dev/null
docker build -t kvfs-cli:dev  --target kvfs-cli  . >/dev/null

echo
echo "=== Network ==="
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

# Tear down anything stale before re-creating (lib helpers don't `rm -f`).
for n in dn1 dn2 dn3 edge; do docker rm -f "$n" >/dev/null 2>&1 || true; done

echo
echo "=== DataNodes ==="
start_dns 3
echo "  ✓ dn1, dn2, dn3"

echo
echo "=== Edge ==="
start_edge edge 8000
echo "  ✓ edge (http://localhost:8000)"

echo
echo "=== Waiting for edge to be healthy ==="
wait_healthz "http://localhost:8000/healthz" && echo "  ✅ edge healthy"

echo
echo "Cluster is up. Try:"
echo "  ./scripts/demo-alpha.sh"
echo "  ./scripts/demo-epsilon.sh"
echo "  ./scripts/down.sh     # teardown"
