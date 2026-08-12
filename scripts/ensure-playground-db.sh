#!/bin/sh
# Create the isolated browser-playground database without touching the normal
# local bot database. Safe to run repeatedly.
set -eu

database=zl_expense_playground
exists=$(docker compose exec -T postgres psql -U postgres -d postgres -tAc \
	"SELECT 1 FROM pg_database WHERE datname = '$database'")
if [ "$exists" != "1" ]; then
	docker compose exec -T postgres createdb -U postgres "$database"
fi
