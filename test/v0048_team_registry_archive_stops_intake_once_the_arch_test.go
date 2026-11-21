//go:build !integration

package test

import (
	"testing"
)

// Requirement: team-registry archive stops intake once the archive event is
// processed. Rejecting webhooks, polled tasks and intake after the event is
// source-controller's and task-manager's behavior (their own tests);
// team-registry's part is that archive changes only its own team row and
// publishes exactly one team_archived event, after commit, that carries what
// their projections need.

// Scenario: Webhook arrives after the archive event is processed.
func TestTeamRegistryArchiveEventCarriesWhatIntakeProjectionsNeed(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	published := len(e.publish(t))
	archived := e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	events := bySubject(e.publish(t)[published:], "team-registry.event.team_archived.pub")
	if len(events) != 1 {
		t.Fatalf("%d team_archived events, want 1", len(events))
	}
	p := mustValidate(t, events[0])
	if asInt(t, p["team_id"]) != id || p["team_name"] != "alpha" || p["archived_at"] != archived["archived_at"] || p["kind"] != "team_archived" {
		t.Fatalf("team_archived %v lacks the team_id, current name or archived_at", p)
	}
	if events[0].Headers["team"] != "admin" {
		t.Fatalf("team header %q, want admin", events[0].Headers["team"])
	}
}

// Scenario: Intake races with archive. team-registry publishes the event only
// through the outbox after the archive commits; nothing reaches the broker
// earlier and nothing is shared with intake.
func TestTeamRegistryArchiveIsPublishedOnlyAfterCommit(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	published := len(e.publish(t))
	commits := e.repo.Commits
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	if e.repo.Commits != commits+1 {
		t.Fatalf("archive used %d commits, want 1", e.repo.Commits-commits)
	}
	if len(e.pub.Messages()) != published {
		t.Fatal("team_archived reached the broker before the relay ran")
	}
	found := false
	for _, r := range e.repo.OutboxRows() {
		if r.Subject == "team-registry.event.team_archived.pub" {
			found = true
		}
	}
	if !found {
		t.Fatal("archive committed without its team_archived outbox row")
	}
}

// Scenario: Existing tasks continue after archive. team-registry owns no task
// and changes nothing but the team's archived_at.
func TestTeamRegistryArchiveChangesNothingButTheTeam(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	other := e.create(t, "beta", imageA)
	before, _ := e.repo.Team(id)
	otherBefore, _ := e.repo.Team(other)
	published := len(e.publish(t))
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	after, _ := e.repo.Team(id)
	if after.ArchivedAt == nil || after.TeamID != before.TeamID || after.TeamName != before.TeamName ||
		after.DefaultImage != before.DefaultImage || !after.IngestedAt.Equal(before.IngestedAt) {
		t.Fatalf("archive changed more than archived_at: %+v -> %+v", before, after)
	}
	if o, _ := e.repo.Team(other); o != otherBefore {
		t.Fatal("archive changed another team")
	}
	msgs := e.publish(t)[published:]
	if len(msgs) != 2 || len(bySubject(msgs, "team-registry.event.team_archived.pub")) != 1 || len(bySubject(msgs, "team-registry.audit.team_archived.pub")) != 1 {
		t.Fatalf("archive published %d messages, want only team_archived event and audit", len(msgs))
	}
	e.mustStatus(t, 200, serviceGet("1", id))
}
