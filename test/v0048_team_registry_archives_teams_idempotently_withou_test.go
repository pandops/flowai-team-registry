//go:build !integration

package test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: team-registry archives teams idempotently without deleting them.

func TestTeamRegistryAdministratorArchivesATeam(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	published := len(e.publish(t))
	body := e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	mustValidateBody(t, "admin.archive.reply.schema.json", body)
	if body["archived_at"] == nil {
		t.Fatal("archive reply has null archived_at")
	}
	if got := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil)); !reflect.DeepEqual(got, body) {
		t.Fatalf("archived team read %v, archive returned %v", got, body)
	}
	events := withPrefix(e.publish(t)[published:], "team-registry.event.")
	if len(events) != 1 || events[0].Subject != "team-registry.event.team_archived.pub" {
		t.Fatalf("events after archive: %v, want exactly one team_archived", events)
	}
	p := mustValidate(t, events[0])
	if p["archived_at"] != body["archived_at"] || asInt(t, p["team_id"]) != id {
		t.Fatalf("team_archived %v does not match the archived team %v", p, body)
	}
}

func TestTeamRegistryArchiveIsRetriedOrDeletionIsAttempted(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	first := e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	published := len(e.publish(t))
	for i := 0; i < 2; i++ {
		again := e.mustStatus(t, 200, adminReq("archive", map[string]any{"team_id": id}, nil))
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("retried archive %v differs from the first %v", again, first)
		}
	}
	msgs := e.publish(t)[published:]
	if events := withPrefix(msgs, "team-registry.event."); len(events) != 0 {
		t.Fatalf("a retried archive published %d events", len(events))
	}
	if audits := bySubject(msgs, "team-registry.audit.team_archived.pub"); len(audits) != 2 {
		t.Fatalf("%d archive audits for two retries, want 2", len(audits))
	}
	for _, s := range team.Subjects() {
		for _, banned := range []string{"unarchive", "delete", "remove", "restore", "list", "scan"} {
			if strings.Contains(s, banned) {
				t.Fatalf("team-registry serves %s", s)
			}
		}
	}
	if _, ok := e.repo.Team(id); !ok {
		t.Fatal("archived team was deleted")
	}
}
