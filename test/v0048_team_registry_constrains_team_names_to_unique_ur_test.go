//go:build !integration

package test

import (
	"strings"
	"sync"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: team-registry constrains team names to unique URL-safe values.

// Scenario: Valid team names are accepted (v0050: `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`,
// 1 to 10 characters). Behaviour matrix M3 "valid create forms".
func TestTeamRegistryValidTeamNamesAreAccepted(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"ops", "qa-2", "team1", "a", "abcdefghij", "a1-b2-c3"} {
		if !team.ValidTeamName(name) {
			t.Fatalf("ValidTeamName(%q) = false", name)
		}
		id := e.create(t, name, imageA)
		stored, ok := e.repo.Team(id)
		if !ok || stored.TeamName != name {
			t.Fatalf("create %q stored %+v, want exactly that name", name, stored)
		}
		got := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
		if got["team_name"] != name {
			t.Fatalf("get of %q returned team_name %v", name, got["team_name"])
		}
	}
}

// Scenario: Team names outside the rule are rejected. Behaviour matrix M3
// "each invalid create form".
func TestTeamRegistryTeamNamesOutsideTheRuleAreRejected(t *testing.T) {
	e := newEnv(t)
	invalid := []string{
		"Acme Corp", "Acme", "1team", "-ops", "ops-", "ops--qa", "ops_qa", "ops.qa", "opsé", "abcdefghijk",
		"ops~qa", "ops qa", "", ".", "..", "a/b", "ops\n", "OPS",
	}
	rows := len(e.repo.OutboxRows())
	for _, name := range invalid {
		if team.ValidTeamName(name) {
			t.Fatalf("ValidTeamName(%q) = true", name)
		}
		mustCode(t, e, 400, "invalid_request", adminReq("create", nil, map[string]any{"team_name": name, "default_image": imageA}))
	}
	if n := len(e.repo.Teams()); n != 0 {
		t.Fatalf("invalid names created %d teams: %+v", n, e.repo.Teams())
	}
	for _, row := range e.repo.OutboxRows()[rows:] {
		if row.Subject != "team-registry.audit.team_created.pub" {
			t.Fatalf("an invalid name produced %s, want only rejected team_created audits", row.Subject)
		}
		if p := decode(t, row.Payload); p["outcome"] != "rejected" || p["reason"] != "invalid_request" || p["team_id"] != nil {
			t.Fatalf("audit of an invalid name %v must be rejected invalid_request and name no team", p)
		}
	}
}

// Scenario: Missing or duplicate team name is rejected. Behaviour matrix M3
// "missing / duplicate active / duplicate archived". A case variant of a
// taken name is no longer a conflict probe: capitals make it 400 by the rule.
func TestTeamRegistryMissingOrDuplicateTeamNameIsRejected(t *testing.T) {
	e := newEnv(t)
	e.create(t, "alpha", imageA)
	archived := e.create(t, "old-team", imageA)
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": archived}, nil))
	mustCode(t, e, 400, "invalid_request", adminReq("create", nil, map[string]any{"default_image": imageA}))
	for _, dup := range []string{"alpha", "old-team"} {
		mustCode(t, e, 409, "team_conflict", adminReq("create", nil, map[string]any{"team_name": dup, "default_image": imageA}))
	}
	if n := len(e.repo.Teams()); n != 2 {
		t.Fatalf("%d teams, want 2: %+v", n, e.repo.Teams())
	}
	if stored, _ := e.repo.Team(archived); stored.TeamName != "old-team" || stored.ArchivedAt == nil {
		t.Fatalf("archived team changed: %+v", stored)
	}
}

// Scenario: Team-name creators race for one unique value (in-process half;
// the real unique index is TestTeamRegistryTeamNameRaceIsDecidedByTheDatabase).
// Renames no longer exist, so only create/create writers race.
func TestTeamRegistryTeamNameWritersRaceForOneUniqueValue(t *testing.T) {
	e := newEnv(t)
	statuses := make([]int, 2)
	codes := make([]string, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int, req team.Request) {
			defer wg.Done()
			rep := e.h.Handle(t.Context(), req)
			statuses[i] = rep.Status
			if rep.Status >= 400 {
				codes[i] = errorCode(decode(t, rep.Body))
			}
		}(i, adminReq("create", nil, map[string]any{"team_name": "contested", "default_image": imageA}))
	}
	wg.Wait()
	wins, losses := 0, 0
	for i, s := range statuses {
		switch {
		case s == 201:
			wins++
		case s == 409 && codes[i] == "team_conflict":
			losses++
		default:
			t.Fatalf("writer %d got %d %s", i, s, codes[i])
		}
	}
	holders := 0
	for _, tm := range e.repo.Teams() {
		if strings.EqualFold(tm.TeamName, "contested") {
			holders++
		}
	}
	if wins != 1 || losses != 1 || holders != 1 {
		t.Fatalf("wins=%d losses=%d holders=%d, want exactly one winner, one 409 and one holder", wins, losses, holders)
	}
}
