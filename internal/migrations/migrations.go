// Package migrations creates and migrates team-registry's own schema.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed sql/*.sql
var files embed.FS

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Apply creates schema if it is missing and applies every pending migration
// inside it. The deployment usually pre-creates the schema for a role that
// has no CREATE right on the database, so CREATE SCHEMA runs only when
// pg_namespace has no such schema. Nothing is created outside schema.
func Apply(ctx context.Context, db *sql.DB, schema string) error {
	if !ident.MatchString(schema) {
		return fmt.Errorf("migrations: invalid schema name %q", schema)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
		return fmt.Errorf("migrations: check schema: %w", err)
	}
	if !exists {
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
			return fmt.Errorf("migrations: create schema %s: %w", schema, err)
		}
	}
	names, err := fs.Glob(files, "sql/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Replicas starting together migrate one after the other.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "flowai-migrate-"+schema); err != nil {
		return fmt.Errorf("migrations: lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+schema+`.schema_migrations (
		version bigint PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migrations: version table: %w", err)
	}
	for _, name := range names {
		base := strings.TrimPrefix(name, "sql/")
		version, err := strconv.ParseInt(strings.SplitN(base, "_", 2)[0], 10, 64)
		if err != nil {
			return fmt.Errorf("migrations: bad file name %s", base)
		}
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+schema+`.schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, strings.ReplaceAll(string(body), "{{schema}}", schema)); err != nil {
			return fmt.Errorf("migrations: %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+schema+`.schema_migrations (version) VALUES ($1)`, version); err != nil {
			return err
		}
	}
	return tx.Commit()
}
