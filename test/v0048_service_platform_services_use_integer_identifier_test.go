//go:build !integration

package test

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
)

// Requirement: service-platform services use integer identifiers. Team
// identifiers are global (the team is the tenant), so "First record in each
// team is 1" has the analog "first team is 1", and "Equal numbers in two
// teams grant nothing" does not apply. "Redelivered message is deduplicated"
// is consumer behavior; team-registry consumes no stream.

func TestTeamRegistryInvalidIdentifierIsRejected(t *testing.T) {
	e := newEnv(t)
	e.create(t, "alpha", imageA)
	for _, id := range []any{"1", 0, -1, 1.5, "abc", nil, true, []int{1}, map[string]int{"id": 1}, 9223372036854775808.0} {
		mustCode(t, e, 400, "invalid_request", adminReq("get", map[string]any{"team_id": id}, nil))
		mustCode(t, e, 400, "invalid_request", serviceGet("admin", id))
	}
	for _, rid := range []string{"", "abc", "1.5", "-", "9223372036854775808", "0x10"} {
		mustCode(t, e, 400, "invalid_request", adminReq("get", map[string]any{"team_id": 1}, nil, hdr{"request_id": rid}))
		req := serviceGet("admin", 1)
		req.Header["request_id"] = rid
		mustCode(t, e, 400, "invalid_request", req)
	}
}

func TestTeamRegistryFirstTeamIsOne(t *testing.T) {
	e := newEnv(t)
	if id := e.create(t, "alpha", imageA); id != 1 {
		t.Fatalf("first team_id %d, want 1", id)
	}
	if id := e.create(t, "beta", imageA); id != 2 {
		t.Fatalf("second team_id %d, want 2", id)
	}
	seq := decode(t, e.publish(t)[0].Data)["publisher_seq"]
	if asInt(t, seq) != 1 {
		t.Fatalf("first publisher_seq %v, want 1", seq)
	}
}

func TestTeamRegistryConcurrentCreatesGetUniqueIncreasingNumbers(t *testing.T) {
	e := newEnv(t)
	const n = 20
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, body := e.do(t, adminReq("create", nil, map[string]any{"team_name": fmt.Sprintf("team-%02d", i), "default_image": imageA}))
			if status != 201 {
				t.Errorf("create %d = %d %v", i, status, body)
				return
			}
			ids[i] = asInt(t, body["team_id"])
		}(i)
	}
	wg.Wait()
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("team ids %v, want 1..%d each once", ids, n)
		}
	}
	seen := map[int64]bool{}
	for _, r := range e.repo.OutboxRows() {
		if seen[r.Seq] {
			t.Fatalf("publisher_seq %d used twice", r.Seq)
		}
		seen[r.Seq] = true
	}
	if len(seen) != 2*n {
		t.Fatalf("%d outbox rows, want %d", len(seen), 2*n)
	}
}

func TestTeamRegistryRolledBackTransactionNeitherPublishesNorReusesItsSequence(t *testing.T) {
	e := newEnv(t)
	e.repo.FailNextCommit(errors.New("commit failed"))
	status, _ := e.do(t, adminReq("create", nil, map[string]any{"team_name": "alpha", "default_image": imageA}))
	if status < 500 {
		t.Fatalf("create with a failed commit = %d, want 5xx", status)
	}
	if len(e.repo.Teams()) != 0 || len(e.repo.OutboxRows()) != 0 || len(e.publish(t)) != 0 {
		t.Fatal("rolled-back create left a team, an outbox row or a message")
	}
	id := e.create(t, "alpha", imageA)
	if id <= 1 {
		t.Fatalf("team_id %d after a rolled-back create: the rolled-back identifier was reused", id)
	}
	if seq := e.repo.OutboxRows()[0].Seq; seq <= 1 {
		t.Fatalf("publisher_seq %d after a rolled-back create: the rolled-back sequence was reused", seq)
	}
}
