# common.sh — shared helpers for kvfs scripts. Source me, don't execute.
#
# Provides:
#   need <cmd>           — die if <cmd> not on PATH
#   sign_url <method> <path> <ttl_seconds> [kid]
#                        — print "<path>?sig=<hex>&exp=<unix>[&kid=<kid>]" using
#                          HMAC-SHA256 over "<method>:<path>:<exp>" with
#                          $SECRET. Optional kid suffix for ADR-049 / Ep.7
#                          (signing with a specific non-primary kid).
#                          Caller must set SECRET in scope before calling.
#
# This file used to be 9× duplicated copies of these two functions across
# every demo + chaos script. Single source of truth here.

need() {
  command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }
}

sign_url() {
  SECRET="$SECRET" python3 -c '
import hmac, hashlib, time, sys, os
secret=os.environ["SECRET"].encode()
method, path, ttl = sys.argv[1], sys.argv[2], int(sys.argv[3])
kid = sys.argv[4] if len(sys.argv) > 4 else ""
exp = int(time.time()) + ttl
sig = hmac.new(secret, f"{method}:{path}:{exp}".encode(), hashlib.sha256).hexdigest()
suffix = f"&kid={kid}" if kid else ""
print(f"{path}?sig={sig}&exp={exp}{suffix}")
' "$1" "$2" "$3" "${4:-}"
}
