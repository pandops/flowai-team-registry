//go:build integration

package test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/pandops/flowai-team-registry/internal/migrations"
	"github.com/pandops/flowai-team-registry/internal/mocks"
	"github.com/pandops/flowai-team-registry/internal/store"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// migrated returns a fresh database migrated as the superuser with the
// default schema, and a store over it.
func migrated(t *testing.T) (*sql.DB, *store.Store) {
	t.Helper()
	db, _ := freshDB(t)
	if err := migrations.Apply(testCtx(t), db, "team_registry_schema"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.New(db, "team_registry_schema")
	if err != nil {
		t.Fatal(err)
	}
	return db, st
}

// pgEnv is a handler and relay over the real store with a fake publisher.
func pgEnv(t *testing.T, st *store.Store) *env {
	t.Helper()
	return newEnvWith(st, st, &mocks.Publisher{})
}

// serviceRole creates a login role with CONNECT on dbName and no CREATE on
// the database. With precreate, the deployment step is reproduced: schema is
// created by the superuser and owned by the role. The role and what it owns
// are dropped at cleanup.
func serviceRole(t *testing.T, admin *sql.DB, dbName, schema string, precreate bool) *sql.DB {
	t.Helper()
	ctx := testCtx(t)
	token, err := randomToken(8)
	if err != nil {
		t.Fatal(err)
	}
	role := "tr_role_" + token
	password, err := randomToken(16)
	if err != nil {
		t.Fatal(err)
	}
	if !isSafeIdentifier(role) || !isSafeIdentifier(schema) {
		t.Fatalf("unsafe identifier %q %q", role, schema)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE`, role, password),
		fmt.Sprintf(`REVOKE ALL ON DATABASE "%s" FROM PUBLIC`, dbName),
		fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO %s`, dbName, role),
		`REVOKE ALL ON SCHEMA public FROM PUBLIC`,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("role setup: %v", err)
		}
	}
	if precreate {
		if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, schema, role)); err != nil {
			t.Fatalf("precreate schema: %v", err)
		}
	}
	db, err := sql.Open("pgx", fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", role, password, shared.host, shared.port, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(c, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, role); err != nil {
			t.Errorf("cleanup: terminate %s: %v", role, err)
		}
		if _, err := admin.ExecContext(c, fmt.Sprintf(`DROP OWNED BY %s CASCADE`, role)); err != nil {
			t.Errorf("cleanup: drop owned by %s: %v", role, err)
		}
		if _, err := admin.ExecContext(c, fmt.Sprintf(`DROP ROLE %s`, role)); err != nil {
			t.Errorf("cleanup: drop role %s: %v", role, err)
		}
	})
	return db
}

func namespaces(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(testCtx(t), `SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

func tablesIn(t *testing.T, db *sql.DB, schema string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(testCtx(t), `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind IN ('r','p','S','v')`, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}
