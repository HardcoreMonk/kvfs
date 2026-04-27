#!/usr/bin/env bash
# demo-pe.sh — Season 7 Ep.3: tunable consistency (ADR-053).
#
# Per-request X-KVFS-W (write quorum) and X-KVFS-R (read quorum) headers
# let the client choose its position on the consistency / availability
# trade-off (Dynamo W+R>N classic). Default: W=⌊R/2⌋+1, R=1 (back-compat).
#
# Demo plan:
#   Stage 1 — sanity: PUT/GET with default quorum, all 3 DN alive.
#   Stage 2 — bad input: X-KVFS-W=99 → 400 Bad Request (validation).
#   Stage 3 — strong write: X-KVFS-W=3 succeeds with all DN alive.
#   Stage 4 — strong write fails: kill 1 DN, retry W=3 → 502.
#   Stage 5 — weak write succeeds: same outage, X-KVFS-W=1 → 200.
#   Stage 6 — strong read: X-KVFS-R=3 with all alive → 200 (agreement).
#   Stage 7 — strong read fails: 1 DN dead, X-KVFS-R=3 → 502 (cannot
#             reach 3-replica agreement).
#   Stage 8 — weak read passes: X-KVFS-R=1 → 200 even with 1 DN dead.
#   Stage 9 — metric counters reflect the explicit overrides.
set -euo pipefail
cd "$(dirname "$0")/.."

. scripts/lib/common.sh
. scripts/lib/cluster.sh

NET="kvfs-net"
SECRET="${EDGE_URLKEY_SECRET:-demo-secret-change-me-in-production}"
EDGE="http://localhost:8000"

echo "=== פ pe demo: tunable consistency (Season 7 Ep.3, ADR-053) ==="
echo

need curl; need jq; need docker; need python3
./scripts/down.sh >/dev/null 2>&1 || true
ensure_images
docker network create $NET 2>/dev/null || true

start_dns 3
COORD_DN_IO=1 start_coord coord1 9000
wait_healthz "http://localhost:9000/v1/coord/healthz"
start_edge edge 8000 "http://coord1:9000"
wait_healthz "${EDGE}/healthz"

# Helper: PUT with optional X-KVFS-W; print HTTP code.
put_with_w() {
  local key="$1" body="$2" w="$3"
  local url="${EDGE}$(sign_url PUT "/v1/o/pe/${key}" 60)"
  local args=(-s -o /dev/null -w '%{http_code}' --max-time 10 -X PUT --data-binary "${body}")
  [ -n "$w" ] && args+=(-H "X-KVFS-W: $w")
  curl "${args[@]}" "$url"
}
get_with_r() {
  local key="$1" r="$2"
  local url="${EDGE}$(sign_url GET "/v1/o/pe/${key}" 60)"
  local args=(-s -o /dev/null -w '%{http_code}' --max-time 10)
  [ -n "$r" ] && args+=(-H "X-KVFS-R: $r")
  curl "${args[@]}" "$url"
}

echo "==> stage 1: default quorum (W=2, R=1) with all 3 DN alive"
RC=$(put_with_w default-w "default-body" "")
echo "    PUT (no header) → ${RC} (expect 201)"
[ "$RC" = "201" ] || { echo "FAIL"; exit 1; }
RC=$(get_with_r default-w "")
echo "    GET (no header) → ${RC} (expect 200)"
[ "$RC" = "200" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 2: invalid header X-KVFS-W=99 (R=3 cluster) → 400"
RC=$(put_with_w bad-w "any" "99")
echo "    PUT W=99 → ${RC} (expect 400)"
[ "$RC" = "400" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 3: strong write W=3 with all alive"
RC=$(put_with_w strong "strong-body" "3")
echo "    PUT W=3 → ${RC} (expect 201)"
[ "$RC" = "201" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 4: kill dn3, then retry W=3 — should fail"
docker stop dn3 >/dev/null
sleep 1
RC=$(put_with_w strong-fail "should-fail" "3")
echo "    PUT W=3 (1 DN dead) → ${RC} (expect 502 — quorum unreachable)"
[ "$RC" = "502" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 5: same outage, weak write W=1 succeeds"
RC=$(put_with_w weak "weak-body" "1")
echo "    PUT W=1 (1 DN dead) → ${RC} (expect 201)"
[ "$RC" = "201" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 6: bring dn3 back. strong read R=3 all-agree."
docker start dn3 >/dev/null
sleep 2
RC=$(get_with_r strong "3")
echo "    GET R=3 (all alive) → ${RC} (expect 200)"
[ "$RC" = "200" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 7: kill dn3 again. strong read R=3 must fail (one replica unreachable)"
docker stop dn3 >/dev/null
sleep 1
RC=$(get_with_r strong "3")
echo "    GET R=3 (1 DN dead) → ${RC} (expect 502)"
[ "$RC" = "502" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 8: weak read R=1 still succeeds with 1 DN dead"
RC=$(get_with_r strong "1")
echo "    GET R=1 (1 DN dead) → ${RC} (expect 200)"
[ "$RC" = "200" ] || { echo "FAIL"; exit 1; }
echo

echo "==> stage 9: metric counters"
docker start dn3 >/dev/null 2>&1 || true
sleep 1
curl -fs "${EDGE}/metrics" 2>/dev/null \
  | grep -E '^kvfs_tunable_quorum_total' | sed 's/^/    /'
echo

echo "=== פ PASS: per-request quorum override controls consistency vs availability ==="
echo "    Cleanup: ./scripts/down.sh"
