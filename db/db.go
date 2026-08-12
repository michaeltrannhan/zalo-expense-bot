// Package db embeds the SQL migration files so binaries are self-contained.
package db

import "embed"

// MigrationsFS holds db/migrations/*.sql.
//
//go:embed all:migrations
var MigrationsFS embed.FS
