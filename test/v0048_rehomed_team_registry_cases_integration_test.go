//go:build integration

package test

// Survivors of deleted State Registry PostgreSQL tests (ledger M210, the
// team-registry part of M279, and the team column constraints of
// M401/M402), on a real PostgreSQL.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pandops/flowai-team-registry/internal/store"
	"github.com/pandops/flowai-team-registry/internal/team"
)

// holdRepo wraps the PostgreSQL store and pauses the first transaction that
// reaches the armed point ("lock": after LockTeam took the row lock;
// "archive": after the archive UPDATE, before commit) until released.
type holdRepo struct {
	inner   *store.Store
	point   string
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func newHoldRepo(st *store.Store, point string) *holdRepo {
	r := &holdRepo{inner: st, point: point, reached: make(chan struct{}), release: make(chan struct{})}
	r.armed.Store(true)
	return r
}

func (r *holdRepo) WithTx(ctx context.Context, fn func(team.Tx) error) error {
	return r.inner.WithTx(ctx, func(tx team.Tx) error { return fn(&holdTx{Tx: tx, r: r}) })
}

func (r *holdRepo) hold(point string) {
	if r.point == point && r.armed.CompareAndSwap(true, false) {
		close(r.reached)
		<-r.release
	}
}

type holdTx struct {
	team.Tx
	r *holdRepo
}

func (x *holdTx) LockTeam(ctx context.Context, id int64) (team.Team, error) {
	t, err := x.Tx.LockTeam(ctx, id)
	if err == nil {
		x.r.hold("lock")
	}
	return t, err
}

func (x *holdTx) ArchiveTeam(ctx context.Context, id int64) (team.Team, error) {
	t, err := x.Tx.ArchiveTeam(ctx, id)
	if err == nil {
		x.r.hold("archive")
	}
	return t, err
}

type outcome struct {
	status int
	body   map[string]any
}

func async(f func() (int, map[string]any)) <-chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		s, b := f()
		ch <- outcome{s, b}
	}()
	return ch
}

func awaitReached(t *testing.T, r *holdRepo) {
	t.Helper()
	select {
	case <-r.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("held writer never reached its hold point")
	}
}

func assertPending(t *testing.T, what string, ch <-chan outcome) {
	t.Helper()
	select {
	case o := <-ch:
		t.Fatalf("%s finished while the row lock is held: %d %v", what, o.status, o.body)
	case <-time.After(300 * time.Millisecond):
	}
}

func receive(t *testing.T, what string, ch <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never finished", what)
		return outcome{}
	}
}

// M210: while an archive transaction is held after its UPDATE, every other
// reader still sees the team active; after commit the archive is 201 once,
// a retry is 200 with the same archived_at, and exactly one team_archived
// event and one succeeded archive audit per request are published.
func TestV0048TeamRegistryArchiveHeldBeforeCommitLeavesTeamActive(t *testing.T) {
	for _, point := range []string{"lock", "archive"} {
		t.Run("held after "+point, func(t *testing.T) {
			db, st := migrated(t)
			plain := pgEnv(t, st)
			id := plain.create(t, "arch-lock", imageA)
			holder := newHoldRepo(st, point)
			held := newEnvWith(holder, st, plain.pub)
			archive := async(func() (int, map[string]any) {
				return held.do(t, adminReq("archive", map[string]any{"team_id": id}, nil))
			})
			awaitReached(t, holder)
			got := plain.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
			if got["archived_at"] != nil {
				t.Fatalf("team read while archive is held: archived_at=%v, want null", got["archived_at"])
			}
			svc := plain.mustStatus(t, 200, serviceGet("admin", id))
			if svc["archived_at"] != nil {
				t.Fatalf("service read while archive is held: %v", svc)
			}
			var committed sql.NullTime
			if err := db.QueryRowContext(testCtx(t), `SELECT archived_at FROM team_registry_schema.teams WHERE team_id = $1`, id).Scan(&committed); err != nil || committed.Valid {
				t.Fatalf("committed archived_at while held = %v (%v), want NULL", committed, err)
			}
			assertPending(t, "held archive", archive)
			close(holder.release)
			first := receive(t, "archive", archive)
			if first.status != 201 || first.body["archived_at"] == nil {
				t.Fatalf("held archive: %d %v, want 201 with archived_at", first.status, first.body)
			}
			retry := plain.mustStatus(t, 200, adminReq("archive", map[string]any{"team_id": id}, nil))
			if retry["archived_at"] != first.body["archived_at"] {
				t.Fatalf("retry archived_at %v, want %v", retry["archived_at"], first.body["archived_at"])
			}
			after := plain.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
			if after["archived_at"] != first.body["archived_at"] {
				t.Fatalf("read after archive: %v", after)
			}
			msgs := plain.publish(t)
			if n := len(bySubject(msgs, "team-registry.event.team_archived.pub")); n != 1 {
				t.Fatalf("%d team_archived events, want 1", n)
			}
			if n := len(bySubject(msgs, "team-registry.audit.team_archived.pub")); n != 2 {
				t.Fatalf("%d archive audits, want 2 (first archive and retry)", n)
			}
		})
	}
}

// M279 (team-registry part of the writer-serialization matrix): two writers
// of one team overlap while the first holds the row lock. The second waits,
// both succeed, the events are published in commit order and the stored team
// is the second writer's result; archive happens once.
func TestV0048TeamRegistryOverlappingTeamWritersSerializeOnTheRowLock(t *testing.T) {
	type writer struct {
		name string
		req  func(id int64) team.Request
		// event the writer publishes on the team's current state, "" for none
		event func(archived bool) string
	}
	// Team names never change (v0050): the overlapping updates are
	// default_image updates with different images.
	image := func(n, img string) writer {
		return writer{"image-" + n, func(id int64) team.Request {
			return adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": img})
		}, func(bool) string { return "team_updated" }}
	}
	archive := writer{"archive", func(id int64) team.Request {
		return adminReq("archive", map[string]any{"team_id": id}, nil)
	}, func(archived bool) string {
		if archived {
			return ""
		}
		return "team_archived"
	}}
	imageC := "registry.example/flowai/runtime-c@sha256:" + digest64
	writers := []writer{image("b", imageB), image("c", imageC), archive}
	for _, a := range writers {
		for _, b := range writers {
			if a.name == b.name && a.name != "archive" {
				continue
			}
			t.Run(a.name+" then "+b.name, func(t *testing.T) {
				_, st := migrated(t)
				plain := pgEnv(t, st)
				id := plain.create(t, "contested", imageA)
				holder := newHoldRepo(st, "lock")
				held := newEnvWith(holder, st, plain.pub)
				first := async(func() (int, map[string]any) { return held.do(t, a.req(id)) })
				awaitReached(t, holder)
				second := async(func() (int, map[string]any) { return plain.do(t, b.req(id)) })
				assertPending(t, "second writer", second)
				close(holder.release)
				r1, r2 := receive(t, a.name, first), receive(t, b.name, second)
				for _, r := range []outcome{r1, r2} {
					if r.status != 200 && r.status != 201 {
						t.Fatalf("writer reply %d %v, want 2xx", r.status, r.body)
					}
				}
				var want []string
				if ev := a.event(false); ev != "" {
					want = append(want, ev)
				}
				if ev := b.event(a.name == "archive"); ev != "" {
					want = append(want, ev)
				}
				var got []string
				for _, m := range withPrefix(plain.publish(t), "team-registry.event.") {
					body := mustValidate(t, m)
					if body["kind"] == "team_created" {
						continue
					}
					got = append(got, body["kind"].(string))
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("events %v, want %v in commit order", got, want)
				}
				final := plain.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
				if final["team_name"] != r2.body["team_name"] || final["default_image"] != r2.body["default_image"] || final["archived_at"] != r2.body["archived_at"] {
					t.Fatalf("stored team %v is not the second (last committed) writer's result %v", final, r2.body)
				}
				if a.name == "archive" && r2.body["archived_at"] != r1.body["archived_at"] {
					t.Fatalf("archived_at moved after the archive: %v then %v", r1.body["archived_at"], r2.body["archived_at"])
				}
			})
		}
	}
}

// M401/M402 column constraints: team_name and default_image are NOT NULL,
// archived_at is nullable, and a NULL insert changes nothing.
func TestV0048TeamRegistryTeamColumnConstraints(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	id := e.create(t, "constrain", imageA)
	for _, q := range []string{
		`INSERT INTO team_registry_schema.teams (team_name, default_image) VALUES ('no-image', NULL)`,
		`INSERT INTO team_registry_schema.teams (team_name, default_image) VALUES (NULL, 'x@sha256:aa')`,
		fmt.Sprintf(`UPDATE team_registry_schema.teams SET default_image = NULL WHERE team_id = %d`, id),
	} {
		_, err := db.ExecContext(testCtx(t), q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
			t.Fatalf("%s: %v, want not_null_violation 23502", q, err)
		}
	}
	var nullable string
	if err := db.QueryRowContext(testCtx(t), `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'team_registry_schema' AND table_name = 'teams' AND column_name = 'archived_at'`).Scan(&nullable); err != nil || nullable != "YES" {
		t.Fatalf("archived_at is_nullable=%q (%v), want YES", nullable, err)
	}
	var n int
	if err := db.QueryRowContext(testCtx(t), `SELECT count(*) FROM team_registry_schema.teams`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d teams after refused writes (%v), want 1", n, err)
	}
	got := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
	if got["default_image"] != imageA {
		t.Fatalf("team after refused NULL image: %v", got)
	}
}
