#!/bin/sh
# Create an isolated PostgreSQL database without touching the normal
# local bot database. Safe to run repeatedly.
# Usage: ensure-db.sh DATABASE_NAME
set -eu

database=${1:?usage: ensure-db.sh DATABASE_NAME}
exists=$(docker compose exec -T postgres psql -U postgres -d postgres -tAc \
	"SELECT 1 FROM pg_database WHERE datname = '$database'")
if [ "$exists" != "1" ]; then
	docker compose exec -T postgres createdb -U postgres "$database"
fi
