#!/bin/sh
# Recreate only the dedicated real-provider E2E database. This intentionally
# destroys prior E2E rows so the same receipt can be tested repeatedly; it
# never accepts a database name or URL from user input.
set -eu

database=zl_expense_e2e
e2e_data_dir=./data/e2e
repo_root=$(pwd -P)

# This reset is intentionally anchored to this repository and a fixed test
# directory. Refuse symlinks so cleanup can never traverse an operator-chosen
# target. find does not follow nested symlinks.
if [ ! -f ./go.mod ] || ! grep -q '^module zl-expese-bot$' ./go.mod; then
	printf '%s\n' 'Refusing E2E reset outside the zl-expese-bot repository.' >&2
	exit 2
fi
if [ -L ./data ] || [ -L "$e2e_data_dir" ]; then
	printf '%s\n' 'Refusing E2E reset because an object-directory path component is a symbolic link.' >&2
	exit 2
fi
mkdir -p "$e2e_data_dir"
data_root=$(cd ./data && pwd -P)
object_root=$(cd "$e2e_data_dir" && pwd -P)
if [ "$data_root" != "$repo_root/data" ] || [ "$object_root" != "$repo_root/data/e2e" ]; then
	printf '%s\n' 'Refusing E2E reset because the object directory resolves outside the repository.' >&2
	exit 2
fi
find "$e2e_data_dir" -depth -mindepth 1 -delete

docker compose exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -c \
	"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$database' AND pid <> pg_backend_pid()" \
	>/dev/null
docker compose exec -T postgres dropdb -U postgres --if-exists "$database"
docker compose exec -T postgres createdb -U postgres "$database"
