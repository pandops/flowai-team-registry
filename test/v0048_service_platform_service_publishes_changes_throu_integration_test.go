//go:build integration

package test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/migrations"
	"github.com/pandops/flowai-team-registry/internal/store"
)

// Requirement: team-registry publishes team changes for downstream
// projections — Consumers receive team changes in order, under concurrent
// writers on one team.
func TestTeamRegistryConcurrentUpdatesOfOneTeamPublishInCommitOrder(t *testing.T) {
	db, st := migrated(t)
	e := pgEnv(t, st)
	id := e.create(t, "start", imageA)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, body := e.do(t, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": fmt.Sprintf("registry.example/flowai/runtime-%d@sha256:%s", i, digest64)}))
			if status != 200 {
				t.Errorf("update %d: %d %v", i, status, body)
			}
		}(i)
	}
	wg.Wait()
	events := bySubject(e.publish(t), "team-registry.event.team_updated.pub")
	if len(events) != 10 {
		t.Fatalf("%d update events, want 10", len(events))
	}
	var prev int64
	var last string
	for _, m := range events {
		p := decode(t, m.Data)
		seq := asInt(t, p["publisher_seq"])
		if seq <= prev {
			t.Fatalf("update events out of order: %d after %d", seq, prev)
		}
		prev = seq
		if p["team_name"] != "start" {
			t.Fatalf("update event carries team_name %v, want the unchanged start", p["team_name"])
		}
		last, _ = p["default_image"].(string)
	}
	var stored string
	if err := db.QueryRowContext(testCtx(t), `SELECT default_image FROM team_registry_schema.teams WHERE team_id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if last != stored {
		t.Fatalf("last published image %q, stored %q: a projection replaying in order would diverge", last, stored)
	}
}

// Integration row "publisher_seq ... a restart continues the sequence": after
// a restart (new pool, new Store, new handler and relay) the next change gets
// a team_id and publisher_seq values above every earlier one, so no
// Nats-Msg-Id is ever reused.
func TestTeamRegistryRestartContinuesTheSequences(t *testing.T) {
	db, dbName := freshDB(t)
	if err := migrations.Apply(testCtx(t), db, "team_registry_schema"); err != nil {
		t.Fatal(err)
	}
	st, _ := store.New(db, "team_registry_schema")
	before := pgEnv(t, st)
	first := before.create(t, "alpha", imageA)
	before.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": first}, map[string]any{"default_image": imageB}))
	earlier := before.publish(t)
	if len(earlier) != 4 {
		t.Fatalf("%d messages before the restart, want 4", len(earlier))
	}
	var maxSeq int64
	ids := map[string]bool{}
	for _, m := range earlier {
		ids[m.MsgID] = true
		if s := asInt(t, decode(t, m.Data)["publisher_seq"]); s > maxSeq {
			maxSeq = s
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: a new process on the same database.
	db2, err := sql.Open("pgx", shared.adminDSN(dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := migrations.Apply(testCtx(t), db2, "team_registry_schema"); err != nil {
		t.Fatalf("migrate after restart: %v", err)
	}
	st2, err := store.New(db2, "team_registry_schema")
	if err != nil {
		t.Fatal(err)
	}
	after := pgEnv(t, st2)
	second := after.create(t, "gamma", imageA)
	if second <= first {
		t.Fatalf("team_id %d after the restart, earlier %d: the identity started again", second, first)
	}
	later := after.publish(t)
	if len(later) != 2 {
		t.Fatalf("%d messages after the restart, want event and audit of the create", len(later))
	}
	for _, m := range later {
		seq := asInt(t, decode(t, m.Data)["publisher_seq"])
		if seq <= maxSeq {
			t.Fatalf("publisher_seq %d after the restart, earlier maximum %d: the sequence started again", seq, maxSeq)
		}
		if ids[m.MsgID] {
			t.Fatalf("Nats-Msg-Id %s reused after the restart", m.MsgID)
		}
		ids[m.MsgID] = true
	}
}
