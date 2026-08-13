#!/bin/sh
# Load repo .env the way a shell does (systemd EnvironmentFile does not),
# then exec the given command. Used by the VM systemd units.
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"
if [ -f "$root/.env" ]; then
	tmp=$(mktemp)
	# Drop CR so KEY\r is not a different name than KEY.
	tr -d '\r' < "$root/.env" > "$tmp"
	set -a
	# shellcheck disable=SC1090
	. "$tmp"
	set +a
	rm -f "$tmp"
fi
exec "$@"
