//go:build !integration

package test

import (
	"sort"
	"strings"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: service-platform service trusts team context only from ui
// requests. team-registry serves no ui subject, so "Gateway forwards
// verified operator context" has no team-registry case; its analog is the
// service point read, where the team header is the only authority.

func TestTeamRegistryServesExactlyItsDocumentedSubjects(t *testing.T) {
	got := append([]string(nil), team.Subjects()...)
	sort.Strings(got)
	want := []string{
		"team-registry.admin.archive.sub",
		"team-registry.admin.create.sub",
		"team-registry.admin.get.sub",
		"team-registry.admin.update.sub",
		"team-registry.service.get.sub",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("served subjects %v, want %v (no ui subject, no list or paginated read)", got, want)
	}
	if team.QueueGroup != "team-registry" {
		t.Fatalf("queue group %q", team.QueueGroup)
	}
}

func TestTeamRegistryPayloadNamesAnotherTeam(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	three := e.create(t, "team-three", imageA)
	four := e.create(t, "team-four", imageB)
	body := e.mustStatus(t, 200, serviceGet("3", three))
	if body["team_name"] != "team-three" {
		t.Fatalf("own team read %v", body)
	}
	status, body := e.do(t, serviceGet("3", four))
	if status != 400 || errorCode(body) != "invalid_request" {
		t.Fatalf("team 3 reading team 4 = %d %v, want 400 invalid_request", status, body)
	}
	if strings.Contains(string(mustJSON(t, body)), "team-four") {
		t.Fatal("team 4 data leaked to team 3")
	}
}

func TestTeamRegistryContextIsMissing(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	rows := len(e.repo.OutboxRows())
	mustCode(t, e, 400, "invalid_request", serviceGet("", id))
	noRequestID := serviceGet("admin", id)
	delete(noRequestID.Header, "request_id")
	// D12 owner decision 2026-11-09: no valid request_id, no request_id
	// field; every other error keeps it.
	if body := mustCode(t, e, 400, "invalid_request", noRequestID); hasKey(body, "request_id") {
		t.Fatalf("service read without request_id answered %v, want no request_id field", body)
	}
	for _, missing := range []string{"team", "request_id", "admin_subject"} {
		body := mustCode(t, e, 400, "invalid_request", adminReq("get", map[string]any{"team_id": id}, nil, hdr{missing: "<delete>"}))
		if body["team_name"] != nil {
			t.Fatalf("request without %s read team data", missing)
		}
		if has := hasKey(body, "request_id"); has != (missing != "request_id") {
			t.Fatalf("request without %s answered %v: request_id present = %v", missing, body, has)
		}
	}
	for _, bad := range []string{"0", "-1", "07", "r-1", "9223372036854775808"} {
		body := mustCode(t, e, 400, "invalid_request", adminReq("get", map[string]any{"team_id": id}, nil, hdr{"request_id": bad}))
		if hasKey(body, "request_id") {
			t.Fatalf("request_id %q answered %v, want no request_id field", bad, body)
		}
	}
	mustCode(t, e, 400, "invalid_request", adminReq("get", map[string]any{"team_id": id}, nil, hdr{"team": "3"}))
	if got := len(e.repo.OutboxRows()); got != rows {
		t.Fatalf("%d outbox rows written for requests without context", got-rows)
	}
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
