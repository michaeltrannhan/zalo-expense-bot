#!/bin/sh
# Repair a glued .env (LOG_LEVEL=infoZALO_WEBHOOK_SECRET=...) and replace
# the development webhook placeholder. Safe to re-run.
set -eu
cd "$(dirname "$0")/.."
if [ ! -f .env ]; then
	echo "fix-env: .env missing" >&2
	exit 1
fi
python3 - <<'PY'
from pathlib import Path
import re, secrets
p = Path(".env")
t = p.read_text(encoding="utf-8", errors="replace").replace("\r\n", "\n").replace("\r", "\n")
t = re.sub(r"(?<=[a-z0-9\"])([A-Z][A-Z0-9_]+=)", r"\n\1", t)
if not t.endswith("\n"):
    t += "\n"
if re.search(r"^ZALO_WEBHOOK_SECRET=dev-secret-change-me\s*$", t, re.M):
    secret = secrets.token_urlsafe(24)
    t = re.sub(
        r"^ZALO_WEBHOOK_SECRET=dev-secret-change-me\s*$",
        "ZALO_WEBHOOK_SECRET=" + secret,
        t,
        flags=re.M,
    )
    print("replaced placeholder ZALO_WEBHOOK_SECRET")
p.write_text(t, encoding="utf-8")
print("rewrote .env")
PY
chmod 600 .env
echo "LOG_LEVEL / ZALO_WEBHOOK_SECRET lines:"
grep -E '^(LOG_LEVEL|ZALO_WEBHOOK_SECRET|APP_ENV|DATABASE_URL|OBJECTSTORE)=' .env
