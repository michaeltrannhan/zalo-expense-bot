#!/bin/sh
# Rebuild the VM pilot stack after git pull: postgres, migrations, binaries,
# then restart systemd units when they are installed.
# Usage: sh scripts/vm-rebuild.sh
set -eu
cd "$(dirname "$0")/.."

if ! docker info >/dev/null 2>&1; then
	echo "vm-rebuild: cannot talk to Docker. Is the daemon up, and are you in the docker group?" >&2
	echo "  sudo systemctl enable --now docker && sudo usermod -aG docker \"\$USER\"" >&2
	echo "  then SSH out and back in (or: exec sg docker -c 'bash -l')" >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	echo "vm-rebuild: go not on PATH (need 1.25+). See docs/vm-pilot.md" >&2
	exit 1
fi

echo ">> postgres + minio"
docker compose up -d postgres minio minio-init

echo ">> migrate + build"
make migrate
make build

units="zl-expense-api zl-expense-receipt-worker zl-expense-notification-worker"
if systemctl list-unit-files zl-expense-api.service >/dev/null 2>&1 &&
	systemctl is-enabled zl-expense-api.service >/dev/null 2>&1; then
	echo ">> restart systemd units"
	sudo systemctl restart $units
	sudo systemctl --no-pager --full status $units || true
	echo "health: curl -sS http://127.0.0.1:8080/healthz"
else
	echo "systemd units not installed yet. First time:"
	echo "  sh scripts/install-vm-services.sh"
	echo "Or run in the foreground:"
	echo "  make run-local API_FLAGS=-poll"
fi
