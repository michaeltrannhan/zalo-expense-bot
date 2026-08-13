#!/bin/sh
# Install systemd units for api + both workers. Run from anywhere; paths
# are taken from this repo checkout. Re-run after moving the clone.
set -eu
cd "$(dirname "$0")/.."
ROOT=$(pwd)
USER_NAME=$(id -un)

if [ ! -f "$ROOT/.env" ]; then
	echo "install-vm-services: $ROOT/.env is missing (copy .env.example first)" >&2
	exit 1
fi
if [ ! -x "$ROOT/bin/api" ] || [ ! -x "$ROOT/bin/receipt-worker" ] || [ ! -x "$ROOT/bin/notification-worker" ]; then
	echo "install-vm-services: binaries missing; run: make build" >&2
	exit 1
fi

for unit in api receipt-worker notification-worker; do
	src="deploy/systemd/zl-expense-${unit}.service"
	sed -e "s|@ROOT@|$ROOT|g" -e "s|@USER@|$USER_NAME|g" "$src" |
		sudo tee "/etc/systemd/system/zl-expense-${unit}.service" >/dev/null
done

sudo systemctl daemon-reload
sudo systemctl enable --now \
	zl-expense-api \
	zl-expense-receipt-worker \
	zl-expense-notification-worker
echo "installed and started: api, receipt-worker, notification-worker"
echo "logs: journalctl -u zl-expense-api -u zl-expense-receipt-worker -u zl-expense-notification-worker -f"
