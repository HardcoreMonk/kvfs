#!/usr/bin/env bash
# down.sh — tear down kvfs cluster (containers + network + volumes).
set -euo pipefail
for c in edge edge1 edge2 edge3 dn1 dn2 dn3 dn4 dn5 dn6 dn7 dn8 coord1 coord2 coord3; do
  docker rm -f "$c" >/dev/null 2>&1 || true
done
docker network rm kvfs-net >/dev/null 2>&1 || true
for v in dn1-data dn2-data dn3-data dn4-data dn5-data dn6-data dn7-data dn8-data \
         edge-data edge1-data edge2-data edge3-data coord1-data coord2-data coord3-data; do
  docker volume rm "$v" >/dev/null 2>&1 || true
done
echo "cluster torn down"
