//go:build !integration

package test

import (
	"errors"
	"testing"
)

// Requirement: service-platform service publishes changes through an outbox.
// "Synchronous publications without an outbox" names source-controller and
// admin-panel; team-registry publishes everything through its outbox, which
// the tests below and the team-registry outbox tests show.

func TestTeamRegistryRolledBackChangePublishesNothing(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	rows := e.repo.OutboxRows()
	last := rows[len(rows)-1].Seq
	e.repo.FailNextCommit(errors.New("commit failed"))
	status, body := e.do(t, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
	if status < 500 {
		t.Fatalf("update with a failed commit = %d, want 5xx", status)
	}
	if body["team_name"] != nil || body["default_image"] != nil {
		t.Fatalf("failed update returned team data: %v", body)
	}
	if got := len(e.repo.OutboxRows()); got != len(rows) {
		t.Fatalf("%d outbox rows after the rollback, want %d", got, len(rows))
	}
	msgs := e.publish(t)
	for _, m := range msgs {
		if p := decode(t, m.Data); p["kind"] == "team_updated" || p["action"] == "team_updated" {
			t.Fatalf("rolled-back update reached the broker on %s", m.Subject)
		}
	}
	if tm, _ := e.repo.Team(id); tm.TeamName != "alpha" || tm.DefaultImage != imageA {
		t.Fatalf("team %+v after rollback, want alpha with the previous image", tm)
	}
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
	after := e.repo.OutboxRows()
	if next := after[len(rows)].Seq; next <= last+1 {
		t.Fatalf("publisher_seq %d after a rollback following %d: the rolled-back sequence was reused", next, last)
	}
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}
