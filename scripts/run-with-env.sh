#!/bin/sh
# Load repo .env the way a shell does (systemd EnvironmentFile does not),
# then exec the given command. Used by the VM systemd units.
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"
if [ -f "$root/.env" ]; then
	tmp=$(mktemp)
	# Drop CR, then insert a newline when two KEY= assignments were glued
	# (e.g. LOG_LEVEL=infoZALO_WEBHOOK_SECRET=...).
	tr -d '\r' < "$root/.env" | sed -E 's/([a-z0-9"])([A-Z][A-Z0-9_]+=)/\1\n\2/g' > "$tmp"
	set -a
	# shellcheck disable=SC1090
	. "$tmp"
	set +a
	rm -f "$tmp"
fi
exec "$@"
