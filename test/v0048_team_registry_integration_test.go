//go:build integration

package test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// team-registry capability rows that need a real PostgreSQL: uniqueness under
// concurrent creates (an archived team keeps its name), the database-level
// immutability of team_id, and archive idempotency in the stored row. Team
// names never change after create (v0050), so there is no rename race.

// Requirement: team-registry constrains team names to unique URL-safe values —
// Team-name creators race for one unique value; the archived holder keeps it.
func TestTeamRegistryTeamNameRaceIsDecidedByTheDatabase(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	const n = 20
	statuses := make([]int, n)
	codes := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, body := e.do(t, adminReq("create", nil, map[string]any{"team_name": "alpha", "default_image": imageA}))
			statuses[i], codes[i] = status, errorCode(body)
		}(i)
	}
	wg.Wait()
	created := 0
	var winner int64
	for i := range statuses {
		switch {
		case statuses[i] == 201:
			created++
		case statuses[i] == 409 && codes[i] == "team_conflict":
		default:
			t.Fatalf("writer %d: %d %q, want 201 or 409 team_conflict", i, statuses[i], codes[i])
		}
	}
	if created != 1 {
		t.Fatalf("%d teams created for one name, want 1", created)
	}
	var holders int
	if err := db.QueryRowContext(testCtx(t), `SELECT count(*), max(team_id) FROM team_registry_schema.teams WHERE lower(team_name) = 'alpha'`).Scan(&holders, &winner); err != nil || holders != 1 {
		t.Fatalf("%d stored holders of alpha (%v), want 1", holders, err)
	}
	msgs := e.publish(t)
	if got := len(bySubject(msgs, "team-registry.event.team_created.pub")); got != 1 {
		t.Fatalf("%d team_created events, want 1", got)
	}
	if got := len(bySubject(msgs, "team-registry.audit.team_created.pub")); got != n {
		t.Fatalf("%d create audits, want %d (one per writer)", got, n)
	}
	// An archived team keeps its name: it is never released for reuse.
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": winner}, nil))
	mustCode(t, e, 409, "team_conflict", adminReq("create", nil, map[string]any{"team_name": "alpha", "default_image": imageB}))
	var stored string
	if err := db.QueryRowContext(testCtx(t), `SELECT team_name FROM team_registry_schema.teams WHERE team_id = $1 AND archived_at IS NOT NULL`, winner).Scan(&stored); err != nil || stored != "alpha" {
		t.Fatalf("archived team name %q (%v), want alpha kept", stored, err)
	}

	// Uniqueness ignores case, also against a stored (legacy) name in another
	// case that the service did not write and must not rewrite. The legacy row
	// stays active: its events would no longer match the team_name rule.
	var legacy int64
	if err := db.QueryRowContext(testCtx(t), `INSERT INTO team_registry_schema.teams (team_name, default_image) VALUES ('Legacy', $1) RETURNING team_id`, imageA).Scan(&legacy); err != nil {
		t.Fatalf("insert legacy team: %v", err)
	}
	mustCode(t, e, 409, "team_conflict", adminReq("create", nil, map[string]any{"team_name": "legacy", "default_image": imageB}))
	var legacyHolders int
	var legacyName string
	if err := db.QueryRowContext(testCtx(t), `SELECT count(*), max(team_name) FROM team_registry_schema.teams WHERE lower(team_name) = 'legacy'`).Scan(&legacyHolders, &legacyName); err != nil || legacyHolders != 1 || legacyName != "Legacy" {
		t.Fatalf("%d holders of legacy, stored name %q (%v), want exactly one row still named Legacy", legacyHolders, legacyName, err)
	}
}

// Requirement: team-registry stores canonical teams with immutable integer
// identity — the database itself refuses a team_id change.
func TestTeamRegistryDatabaseRefusesATeamIdentifierChange(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	id := e.create(t, "alpha", imageA)
	for _, q := range []string{
		fmt.Sprintf(`UPDATE team_registry_schema.teams SET team_id = 99 WHERE team_id = %d`, id),
		`INSERT INTO team_registry_schema.teams (team_id, team_name, default_image) VALUES (50, 'manual', 'x@sha256:aa')`,
	} {
		_, err := db.ExecContext(testCtx(t), q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s: %v, want a PostgreSQL error", q, err)
		}
	}
	got := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
	if asInt(t, got["team_id"]) != id || got["team_name"] != "alpha" {
		t.Fatalf("team after refused change: %v", got)
	}
}

// Requirement: team-registry archives teams idempotently without deletion —
// the stored archive time does not move on retry.
func TestTeamRegistryArchiveRetryKeepsTheStoredArchiveTime(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	id := e.create(t, "alpha", imageA)
	first := e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	second := e.mustStatus(t, 200, adminReq("archive", map[string]any{"team_id": id}, nil))
	if first["archived_at"] != second["archived_at"] || first["archived_at"] == nil {
		t.Fatalf("archived_at %v then %v", first["archived_at"], second["archived_at"])
	}
	var rows int
	if err := db.QueryRowContext(testCtx(t), `SELECT count(*) FROM team_registry_schema.teams WHERE team_id = $1 AND archived_at IS NOT NULL`, id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("archived row count %d (%v), want 1: archive must not delete", rows, err)
	}
	if got := len(bySubject(e.publish(t), "team-registry.event.team_archived.pub")); got != 1 {
		t.Fatalf("%d archive events, want 1", got)
	}
}
