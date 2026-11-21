//go:build !integration

package test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: team-registry serves the team point read over its service subject.

func TestTeamRegistryServiceReadsOneTeam(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	archived := e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	rows := len(e.repo.OutboxRows())
	for _, teamHeader := range []string{strconv.FormatInt(id, 10), "admin"} {
		body := e.mustStatus(t, 200, serviceGet(teamHeader, id))
		mustValidateBody(t, "service.get.reply.schema.json", body)
		if !reflect.DeepEqual(body, archived) {
			t.Fatalf("service.get (team %s) = %v, want the canonical archived team %v", teamHeader, body, archived)
		}
	}
	if len(e.repo.OutboxRows()) != rows {
		t.Fatal("a service read wrote an outbox row; only admin requests are audited")
	}
}

func TestTeamRegistryTeamPointReadIsUnavailableOrUnknown(t *testing.T) {
	e := newEnv(t)
	e.create(t, "alpha", imageA)
	body := mustCode(t, e, 404, "canonical_team_not_found", serviceGet("42", 42))
	if _, leaked := body["team_name"]; leaked {
		t.Fatal("not-found reply carries team data")
	}
}

func TestTeamRegistryServesNoUISubjectAndChecksTheTeamHeader(t *testing.T) {
	e := newEnv(t)
	a := e.create(t, "alpha", imageA)
	b := e.create(t, "beta", imageA)
	for _, s := range team.Subjects() {
		if strings.Contains(s, ".ui.") || !strings.HasSuffix(s, ".sub") {
			t.Fatalf("team-registry serves %s", s)
		}
	}
	if !subjectsContain(team.Subjects(), "team-registry.service.get.sub") {
		t.Fatal("team-registry does not serve team-registry.service.get.sub")
	}
	body := mustCode(t, e, 400, "invalid_request", serviceGet(strconv.FormatInt(b, 10), a))
	if body["team_name"] != nil {
		t.Fatal("mismatched header returned team data")
	}
	mustCode(t, e, 400, "invalid_request", serviceGet("", a))
	noRequestID := serviceGet(strconv.FormatInt(a, 10), a)
	delete(noRequestID.Header, "request_id")
	mustCode(t, e, 400, "invalid_request", noRequestID)
}
