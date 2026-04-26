# common.sh — shared helpers for kvfs scripts. Source me, don't execute.
#
# Provides:
#   need <cmd>           — die if <cmd> not on PATH
#   sign_url <method> <path> <ttl_seconds>
#                        — print "<path>?sig=<hex>&exp=<unix>" using HMAC-SHA256
#                          over "<method>:<path>:<exp>" with $SECRET as the key.
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
secret=os.environ["SECRET"].encode(); method=sys.argv[1]; path=sys.argv[2]; ttl=int(sys.argv[3])
exp=int(time.time())+ttl
sig=hmac.new(secret,f"{method}:{path}:{exp}".encode(),hashlib.sha256).hexdigest()
print(f"{path}?sig={sig}&exp={exp}")
' "$1" "$2" "$3"
}
