// Command team-registry runs the team-registry service: canonical teams,
// their administration over NATS and the team point read, with every change
// published through its outbox.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pandops/flowai-team-registry/internal/config"
	"github.com/pandops/flowai-team-registry/internal/migrations"
	"github.com/pandops/flowai-team-registry/internal/natsbus"
	"github.com/pandops/flowai-team-registry/internal/outbox"
	"github.com/pandops/flowai-team-registry/internal/probe"
	"github.com/pandops/flowai-team-registry/internal/store"
	"github.com/pandops/flowai-team-registry/internal/team"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "team-registry")
	if err := run(logger); err != nil {
		logger.Error("team-registry stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	path := flag.String("config", "/etc/flowai/config.yaml", "configuration file")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", cfg.DB.DSN())
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	st, err := store.New(db, cfg.DB.Schema)
	if err != nil {
		return err
	}
	bus, err := natsbus.Connect(cfg.NATS, logger)
	if err != nil {
		return fmt.Errorf("broker: %w", err)
	}
	defer bus.Close()

	var migrated atomic.Bool
	ready := func(context.Context) error {
		if !migrated.Load() {
			return errors.New("schema not migrated")
		}
		return nil
	}
	srv := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           probe.Handler("team-registry", ready, st.Ping, bus.Ready),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("probe listener failed", "error", err.Error())
			stop()
		}
	}()
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()

	// The database may come up after the service; retry until migrated.
	for {
		err := migrations.Apply(ctx, db, cfg.DB.Schema)
		if err == nil {
			break
		}
		logger.Warn("migration not applied yet", "error", err.Error())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	migrated.Store(true)

	handler := team.NewHandler(st, time.Now, logger)
	if err := bus.Serve(ctx, team.Subjects(), handler); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	relay := outbox.NewRelay(st, bus, logger, cfg.Outbox.PollInterval)
	logger.Info("team-registry started", "listen", cfg.HTTP.Listen, "schema", cfg.DB.Schema)
	relay.Run(ctx)
	return nil
}
