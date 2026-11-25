//go:build integration

package test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/outbox"
	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: service-platform services use integer identifiers, on the
// real PostgreSQL sequences.

// Scenarios: First record (first team is 1) and Concurrent creates get unique
// increasing numbers.
func TestTeamRegistryConcurrentCreatesOnPostgreSQL(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	const n = 20
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, body := e.do(t, adminReq("create", nil, map[string]any{"team_name": fmt.Sprintf("team-%02d", i), "default_image": imageA}))
			if status != 201 {
				t.Errorf("create %d: %d %v", i, status, body)
				return
			}
			n, _ := body["team_id"].(interface{ Int64() (int64, error) })
			if n != nil {
				ids[i], _ = n.Int64()
			}
		}(i)
	}
	wg.Wait()
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("team ids %v, want 1..%d", ids, n)
		}
	}
	var distinct, total int
	if err := db.QueryRowContext(testCtx(t), `SELECT count(DISTINCT publisher_seq), count(*) FROM team_registry_schema.outbox`).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if distinct != 2*n || total != 2*n {
		t.Fatalf("outbox has %d rows with %d distinct publisher_seq, want %d", total, distinct, 2*n)
	}
}

// Scenario: Rolled-back transaction neither publishes nor reuses its sequence.
func TestTeamRegistryRolledBackTransactionOnPostgreSQL(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	var rolledTeam, rolledSeq int64
	abort := errors.New("abort")
	err := st.WithTx(context.Background(), func(tx team.Tx) error {
		tm, err := tx.InsertTeam(context.Background(), "alpha", imageA)
		if err != nil {
			return err
		}
		rolledTeam = tm.TeamID
		seq, err := tx.NextPublisherSeq(context.Background())
		if err != nil {
			return err
		}
		rolledSeq = seq
		if err := tx.InsertOutbox(context.Background(), outbox.Row{Seq: seq, Subject: "team-registry.event.team_created.pub", Headers: map[string]string{"team": "admin"}, Payload: []byte(`{}`)}); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("WithTx = %v, want the abort error", err)
	}
	var rows int
	if err := db.QueryRowContext(testCtx(t), `SELECT (SELECT count(*) FROM team_registry_schema.teams) + (SELECT count(*) FROM team_registry_schema.outbox)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d rows after rollback (%v), want 0", rows, err)
	}
	if got := len(e.publish(t)); got != 0 {
		t.Fatalf("%d messages after a rolled-back transaction", got)
	}
	id := e.create(t, "alpha", imageA)
	if id <= rolledTeam {
		t.Fatalf("team_id %d after rolled-back %d: reused", id, rolledTeam)
	}
	msgs := e.publish(t)
	for _, m := range msgs {
		if seq := asInt(t, decode(t, m.Data)["publisher_seq"]); seq <= rolledSeq {
			t.Fatalf("publisher_seq %d after rolled-back %d: reused", seq, rolledSeq)
		}
	}
}
