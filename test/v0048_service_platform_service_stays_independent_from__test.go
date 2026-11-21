//go:build !integration

package test

import "testing"

// Requirement: service-platform service stays independent from execution and
// identity. "Executor capacity is not decided centrally" and "Flow cannot be
// created from a Source path" do not apply to team-registry.

func TestTeamRegistryImageEqualityAcrossTeamsGrantsNoAuthority(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	e.create(t, "team-three", imageA)
	e.create(t, "team-four", imageA)
	mustCode(t, e, 400, "invalid_request", serviceGet("3", 4))
	mustCode(t, e, 400, "invalid_request", serviceGet("4", 3))
	mustCode(t, e, 400, "invalid_request", serviceGet(imageA, 4))
	// Changing one team's image leaves the other unchanged.
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": 3}, map[string]any{"default_image": imageB}))
	if got := e.mustStatus(t, 200, serviceGet("4", 4)); got["default_image"] != imageA {
		t.Fatalf("team 4 image %v after team 3 changed its image", got["default_image"])
	}
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": 3}, nil))
	if got := e.mustStatus(t, 200, serviceGet("4", 4)); got["archived_at"] != nil {
		t.Fatal("archiving team 3 archived team 4, which shares its old image")
	}
}
