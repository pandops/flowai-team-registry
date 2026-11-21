//go:build !integration

package test

import "testing"

// Requirement: service-platform service stores team ownership in normalized
// form. A team is its own owner: "Team-owned parent keeps its team" maps to
// the immutable team_id, and "Child references a foreign-team parent" does
// not apply (teams have no child rows in team-registry).

func TestTeamRegistryTeamIdentifierNeverChanges(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	id := e.create(t, "alpha", imageA)
	for _, body := range []map[string]any{{"team_id": 4}, {"team_id": 4, "team_name": "beta"}, {"ingested_at": "2020-01-01T00:00:00.000Z"}, {"archived_at": nil}} {
		mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": id}, body))
	}
	if _, ok := e.repo.Team(4); ok {
		t.Fatal("team 4 appeared")
	}
	tm, _ := e.repo.Team(3)
	if tm.TeamID != 3 || tm.TeamName != "alpha" {
		t.Fatalf("team 3 = %+v", tm)
	}
}

func TestTeamRegistryDisplayMetadataNeverAuthorizes(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	id := e.create(t, "alpha", imageA)
	body := e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
	if asInt(t, body["team_id"]) != 3 || body["team_name"] != "alpha" {
		t.Fatalf("update changed team_id or team_name: %v", body)
	}
	if got := e.mustStatus(t, 200, serviceGet("3", 3)); got["team_name"] != "alpha" || got["default_image"] != imageB {
		t.Fatalf("team 3 after update = %v", got)
	}
	for _, header := range []string{"alpha", "ALPHA", "beta"} {
		mustCode(t, e, 400, "invalid_request", serviceGet(header, 3))
	}
	for _, m := range e.publish(t) {
		if p := mustValidate(t, m); asInt(t, p["team_id"]) != 3 {
			t.Fatalf("%s names team %v", m.Subject, p["team_id"])
		}
	}
}
