//go:build !integration

package test

import (
	"strings"
	"testing"
)

// Requirement: service-platform service sends human actions to audit and
// others to journal. team-registry has no operator actions and no Executor
// events: "Operator cancels a task" and "Executor event goes to journal" do
// not apply; team-registry publishes nothing to journal.

func TestTeamRegistryForwardedAdministratorActionIsAuditedByItsOwner(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	e.create(t, "alpha", imageA)
	before := len(e.publish(t))
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": 3}, nil))
	msgs := e.publish(t)[before:]
	audits := withPrefix(msgs, "team-registry.audit.")
	if len(audits) != 1 || audits[0].Subject != "team-registry.audit.team_archived.pub" {
		t.Fatalf("audits for the archive: %+v", audits)
	}
	a := mustValidate(t, audits[0])
	if audits[0].Headers["team"] != "admin" || a["actor_subject"] != adminSubject || a["actor_type"] != "administrator" ||
		a["outcome"] != "succeeded" || asInt(t, a["resource_id"]) != 3 || a["source_service"] != "team-registry" {
		t.Fatalf("archive audit %v headers %v", a, audits[0].Headers)
	}
	for _, m := range msgs {
		if !strings.HasPrefix(m.Subject, "team-registry.event.") && !strings.HasPrefix(m.Subject, "team-registry.audit.") {
			t.Fatalf("published to %s", m.Subject)
		}
		if strings.Contains(m.Subject, "journal") {
			t.Fatalf("human action sent to journal: %s", m.Subject)
		}
	}
	// Retrying the archive is a read of the archived state: audited, not a second event.
	e.mustStatus(t, 200, adminReq("archive", map[string]any{"team_id": 3}, nil))
	retry := e.publish(t)[before+len(msgs):]
	if len(withPrefix(retry, "team-registry.event.")) != 0 {
		t.Fatal("archive retry published a second event")
	}
}

func TestTeamRegistryRejectedHumanActionIsAudited(t *testing.T) {
	e := newEnv(t)
	e.create(t, "alpha", imageA)
	e.create(t, "beta", imageA)
	before := len(e.publish(t))
	mustCode(t, e, 409, "team_conflict", adminReq("create", nil, map[string]any{"team_name": "alpha", "default_image": imageB}))
	msgs := e.publish(t)[before:]
	if len(msgs) != 1 || msgs[0].Subject != "team-registry.audit.team_created.pub" {
		t.Fatalf("messages for the rejected create: %+v", msgs)
	}
	a := mustValidate(t, msgs[0])
	if a["outcome"] != "rejected" || a["reason"] == nil || a["actor_subject"] != adminSubject {
		t.Fatalf("rejected audit %v", a)
	}
	if n := len(e.repo.Teams()); n != 2 {
		t.Fatalf("rejected create left %d teams, want 2", n)
	}
	if tm, _ := e.repo.Team(1); tm.TeamName != "alpha" || tm.DefaultImage != imageA {
		t.Fatalf("rejected create changed the team: %+v", tm)
	}
}
