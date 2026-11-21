//go:build integration

package test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pandops/flowai-team-registry/internal/migrations"
	"github.com/pandops/flowai-team-registry/internal/store"
)

// Requirement: service-platform service owns one PostgreSQL schema and role.

// Scenario: Default schema name — migration creates and uses only
// team_registry_schema, and a second run changes nothing.
func TestTeamRegistryMigrationCreatesOnlyTheDefaultSchema(t *testing.T) {
	db, _ := freshDB(t)
	before := namespaces(t, db)
	publicBefore := tablesIn(t, db, "public")
	for run := 0; run < 2; run++ {
		if err := migrations.Apply(testCtx(t), db, "team_registry_schema"); err != nil {
			t.Fatalf("migrate run %d: %v", run+1, err)
		}
	}
	after := namespaces(t, db)
	for n := range after {
		if !before[n] && n != "team_registry_schema" {
			t.Fatalf("migration created foreign schema %q", n)
		}
	}
	if !after["team_registry_schema"] {
		t.Fatal("team_registry_schema not created")
	}
	tables := tablesIn(t, db, "team_registry_schema")
	for _, want := range []string{"teams", "outbox"} {
		if !tables[want] {
			t.Fatalf("table %s missing from team_registry_schema: %v", want, tables)
		}
	}
	if got := tablesIn(t, db, "public"); len(got) != len(publicBefore) {
		t.Fatalf("migration wrote to public: %v", got)
	}
}

// Scenario: Configured schema and host are used — with a configured schema
// pre-created for a role that has no CREATE on the database, migration skips
// CREATE SCHEMA and the service stores its rows only there.
func TestTeamRegistryConfiguredSchemaIsUsedByARoleWithoutDatabaseCreate(t *testing.T) {
	admin, dbName := freshDB(t)
	role := serviceRole(t, admin, dbName, "tr_custom_schema", true)
	if _, err := role.ExecContext(testCtx(t), `CREATE SCHEMA tr_probe`); err == nil {
		t.Fatal("service role can create schemas: the fixture does not reproduce the deployment role")
	}
	for run := 0; run < 2; run++ {
		if err := migrations.Apply(testCtx(t), role, "tr_custom_schema"); err != nil {
			t.Fatalf("migrate as the service role, run %d: %v", run+1, err)
		}
	}
	st, err := store.New(role, "tr_custom_schema")
	if err != nil {
		t.Fatal(err)
	}
	e := pgEnv(t, st)
	id := e.create(t, "alpha", imageA)
	var name string
	if err := admin.QueryRowContext(testCtx(t), `SELECT team_name FROM tr_custom_schema.teams WHERE team_id = $1`, id).Scan(&name); err != nil || name != "alpha" {
		t.Fatalf("team row in tr_custom_schema: %q %v", name, err)
	}
	if namespaces(t, admin)["team_registry_schema"] {
		t.Fatal("the default schema was created although another was configured")
	}
	if got := len(e.publish(t)); got != 2 {
		t.Fatalf("%d messages relayed from tr_custom_schema.outbox, want 2", got)
	}
}

// A role without CREATE on the database and no pre-created schema cannot
// migrate: the service fails instead of writing elsewhere.
func TestTeamRegistryMigrationFailsWithoutItsSchemaOrRight(t *testing.T) {
	admin, dbName := freshDB(t)
	role := serviceRole(t, admin, dbName, "team_registry_schema", false)
	if err := migrations.Apply(testCtx(t), role, "team_registry_schema"); err == nil {
		t.Fatal("migration succeeded without a schema and without CREATE on the database")
	}
	if len(tablesIn(t, admin, "public")) != 0 {
		t.Fatal("failed migration left tables in public")
	}
}

// Scenario: Access to a foreign schema is denied.
func TestTeamRegistryAccessToAForeignSchemaIsDenied(t *testing.T) {
	admin, dbName := freshDB(t)
	ctx := testCtx(t)
	for _, stmt := range []string{
		`CREATE SCHEMA task_manager_schema`,
		`CREATE TABLE task_manager_schema.tasks (task_id bigint PRIMARY KEY, team_id bigint NOT NULL)`,
		`INSERT INTO task_manager_schema.tasks VALUES (1010, 3)`,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	role := serviceRole(t, admin, dbName, "team_registry_schema", true)
	if err := migrations.Apply(ctx, role, "team_registry_schema"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM task_manager_schema.tasks`,
		`INSERT INTO task_manager_schema.tasks VALUES (1011, 3)`,
		`UPDATE task_manager_schema.tasks SET team_id = 4`,
		`DELETE FROM task_manager_schema.tasks`,
	} {
		_, err := role.ExecContext(ctx, q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s as the team-registry role: %v, want permission denied (42501)", q, err)
		}
	}
	var team int64
	if err := admin.QueryRowContext(ctx, `SELECT team_id FROM task_manager_schema.tasks WHERE task_id = 1010`).Scan(&team); err != nil || team != 3 {
		t.Fatalf("foreign row changed: %d %v", team, err)
	}
	var n int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM task_manager_schema.tasks`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("foreign table has %d rows (%v), want 1", n, err)
	}
}
