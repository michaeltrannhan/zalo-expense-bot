// Command migrate applies database migrations and exits.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"zl-expese-bot/db"
	"zl-expese-bot/internal/logging"
	"zl-expese-bot/internal/platform/migrate"
	"zl-expese-bot/internal/platform/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	level := strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	if level == "" {
		level = "info"
	}
	log := logging.New(level)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := migrate.Up(ctx, pool, db.MigrationsFS, "migrations")
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		log.Info("schema up to date")
		return nil
	}
	log.Info("migrations applied", slog.Any("versions", applied))
	return nil
}
