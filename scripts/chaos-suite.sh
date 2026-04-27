#!/usr/bin/env bash
# chaos-suite.sh — run all chaos-*.sh scripts in sequence, aggregate
# pass/fail. Each scenario is self-contained (brings up + tears down its
# own cluster), so they can run back-to-back without state coupling.
#
# Usage:
#   ./scripts/chaos-suite.sh                 # full suite, default params
#   ./scripts/chaos-suite.sh --quick         # shorter durations for smoke
#   ./scripts/chaos-suite.sh --skip flap     # exclude one scenario
#
# Exit code: 0 if every scenario PASS; 1 if any FAIL.
set -u
cd "$(dirname "$0")/.."

QUICK=0
SKIP=""
while [ $# -gt 0 ]; do
  case "$1" in
    --quick) QUICK=1; shift ;;
    --skip)  SKIP="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,11p' "$0" | sed 's|^# \{0,1\}||'
      exit 0 ;;
    *) echo "unknown flag: $1"; exit 2 ;;
  esac
done

# scenario_name | script_path | quick_args | full_args
SCENARIOS=(
  "flap|./scripts/chaos-coord-flap.sh|--duration 30 --interval 8 --downtime 3|--duration 90 --interval 12 --downtime 4"
  "quorum-loss|./scripts/chaos-coord-quorum-loss.sh|--down-sec 6|--down-sec 8"
  "partition|./scripts/chaos-coord-partition.sh|--duration 30 --cycles 1 --partition-sec 8|--duration 60 --cycles 2 --partition-sec 10"
  "mixed|./scripts/chaos-mixed.sh|--duration 40 --interval 10 --downtime 3|--duration 90 --interval 12 --downtime 4"
  "dn-killer|./scripts/chaos-dn-killer.sh|--duration 30 --interval 6|--duration 60 --interval 10"
)

# dn-killer expects a pre-existing cluster (legacy contract). Bring one up
# briefly for it.
ensure_cluster_for_dn_killer() {
  if ! curl -fsS --max-time 2 "http://localhost:8000/healthz" >/dev/null 2>&1; then
    echo "    [pre] no cluster running — bringing up via ./scripts/up.sh"
    ./scripts/up.sh >/dev/null 2>&1 || {
      echo "    [pre] up.sh failed; skipping dn-killer"
      return 1
    }
  fi
  return 0
}

PASS=()
FAIL=()
SKIPPED=()

for entry in "${SCENARIOS[@]}"; do
  IFS='|' read -r name script quick_args full_args <<< "$entry"
  if [ "$SKIP" = "$name" ]; then
    SKIPPED+=("$name")
    continue
  fi

  echo
  echo "=========================================="
  echo "  scenario: ${name}"
  echo "=========================================="

  if [ "$name" = "dn-killer" ]; then
    if ! ensure_cluster_for_dn_killer; then
      SKIPPED+=("$name")
      continue
    fi
  fi

  if [ "$QUICK" = "1" ]; then
    args="$quick_args"
  else
    args="$full_args"
  fi

  if "$script" $args; then
    PASS+=("$name")
  else
    FAIL+=("$name")
    echo
    echo "  ❌ ${name} FAILED — continuing to next scenario"
  fi
done

# Tidy: tear down anything dn-killer left behind.
./scripts/down.sh >/dev/null 2>&1 || true

echo
echo "=========================================="
echo "  chaos suite summary"
echo "=========================================="
echo "   PASS:    ${#PASS[@]}  ${PASS[*]:-}"
echo "   FAIL:    ${#FAIL[@]}  ${FAIL[*]:-}"
echo "   SKIPPED: ${#SKIPPED[@]}  ${SKIPPED[*]:-}"

[ "${#FAIL[@]}" -eq 0 ] || exit 1
