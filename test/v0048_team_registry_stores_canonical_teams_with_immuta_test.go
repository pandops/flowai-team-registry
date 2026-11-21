//go:build !integration

package test

import (
	"encoding/json"
	"sort"
	"testing"
)

// Requirement: team-registry stores canonical teams with immutable identity.

func TestTeamRegistryCreatedTeamHasGeneratedIdentityAndNoIdentityProviderData(t *testing.T) {
	e := newEnv(t)
	body := e.mustStatus(t, 201, adminReq("create", nil, map[string]any{"team_name": "alpha", "default_image": imageA}))
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"archived_at", "default_image", "ingested_at", "team_id", "team_name"}
	if len(keys) != len(want) {
		t.Fatalf("reply keys %v, want exactly %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("reply keys %v, want exactly %v", keys, want)
		}
	}
	id := asInt(t, body["team_id"])
	if id != 1 {
		t.Fatalf("first team_id = %d, want 1 (first value of the global sequence)", id)
	}
	if body["archived_at"] != nil {
		t.Fatalf("archived_at = %v, want null", body["archived_at"])
	}
	if body["team_name"] != "alpha" || body["default_image"] != imageA {
		t.Fatalf("reply %v does not echo the stored fields", body)
	}
	mustValidateBody(t, "admin.create.reply.schema.json", body)
	stored, ok := e.repo.Team(id)
	if !ok || stored.TeamName != "alpha" || stored.DefaultImage != imageA || stored.ArchivedAt != nil {
		t.Fatalf("stored team %+v (found %v) does not match the created team", stored, ok)
	}
}

func TestTeamRegistryTeamIdentifiersIncreaseAcrossTeams(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(41)
	alphaBody := e.mustStatus(t, 201, adminReq("create", nil, map[string]any{"team_name": "alpha", "default_image": imageA}))
	if _, isNumber := alphaBody["team_id"].(json.Number); !isNumber {
		t.Fatalf("team_id %#v is not a JSON number", alphaBody["team_id"])
	}
	alpha := asInt(t, alphaBody["team_id"])
	beta := e.create(t, "beta", imageA)
	if alpha <= 41 || beta <= alpha {
		t.Fatalf("alpha=%d beta=%d, want 41 < alpha < beta", alpha, beta)
	}
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": alpha}, nil))
	gamma := e.create(t, "gamma", imageA)
	if gamma <= beta || gamma == alpha {
		t.Fatalf("gamma=%d reuses or does not follow earlier numbers (alpha=%d archived, beta=%d)", gamma, alpha, beta)
	}
}

func TestTeamRegistryRequestCarryingIdentityProviderDataIsRejected(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	before, _ := e.repo.Team(id)
	rowsBefore := len(e.repo.OutboxRows())
	forbidden := []string{"oidc_issuer", "issuer", "iss", "sub", "oidc_subject", "oidc_team_id", "team_id", "operator_id", "operators", "members", "membership"}
	for _, field := range forbidden {
		createBody := map[string]any{"team_name": "beta", "default_image": imageA, field: "value-1"}
		mustCode(t, e, 400, "invalid_request", adminReq("create", nil, createBody))
		updateBody := map[string]any{"default_image": imageB, field: "value-1"}
		mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": id}, updateBody))
	}
	if n := len(e.repo.Teams()); n != 1 {
		t.Fatalf("%d teams stored, want 1: a rejected request persisted a team", n)
	}
	if after, _ := e.repo.Team(id); after.TeamName != before.TeamName || after.DefaultImage != before.DefaultImage {
		t.Fatalf("team changed by a rejected update: %+v -> %+v", before, after)
	}
	for _, row := range e.repo.OutboxRows()[rowsBefore:] {
		if row.Subject != "team-registry.audit.team_created.pub" && row.Subject != "team-registry.audit.team_updated.pub" {
			t.Fatalf("rejected request wrote %s; only rejected audit rows are allowed", row.Subject)
		}
		payload := decode(t, row.Payload)
		if payload["outcome"] != "rejected" {
			t.Fatalf("audit for a rejected request has outcome %v", payload["outcome"])
		}
		for _, field := range forbidden[:len(forbidden)-0] {
			if field == "team_id" {
				continue
			}
			if _, leaked := payload[field]; leaked {
				t.Fatalf("audit row carries identity-provider field %s", field)
			}
		}
	}
}
