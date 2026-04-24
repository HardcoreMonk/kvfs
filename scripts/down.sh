#!/usr/bin/env bash
# down.sh — tear down kvfs cluster (containers + network + volumes).
set -euo pipefail
for c in edge dn1 dn2 dn3; do
  docker rm -f "$c" >/dev/null 2>&1 || true
done
docker network rm kvfs-net >/dev/null 2>&1 || true
for v in dn1-data dn2-data dn3-data edge-data; do
  docker volume rm "$v" >/dev/null 2>&1 || true
done
echo "cluster torn down"
