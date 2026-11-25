//go:build !integration

package test

import (
	"encoding/json"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
	"github.com/pandops/flowai-team-registry/schemas"
)

// Requirement: team-registry requires a mutable digest-bearing team default image.

func TestTeamRegistryAdministratorChangesTheTeamDefaultImage(t *testing.T) {
	e := newEnv(t)
	active := e.create(t, "alpha", imageA)
	archived := e.create(t, "beta", imageA)
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": archived}, nil))
	published := len(e.publish(t))
	for _, id := range []int64{active, archived} {
		body := e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
		if body["default_image"] != imageB {
			t.Fatalf("reply image %v, want %s verbatim", body["default_image"], imageB)
		}
		if stored, _ := e.repo.Team(id); stored.DefaultImage != imageB {
			t.Fatalf("stored image %q, want %q verbatim", stored.DefaultImage, imageB)
		}
	}
	events := bySubject(e.publish(t)[published:], "team-registry.event.team_updated.pub")
	if len(events) != 2 {
		t.Fatalf("%d team_updated events, want 2", len(events))
	}
	for i, ev := range events {
		p := mustValidate(t, ev)
		if p["default_image"] != imageB || asInt(t, p["team_id"]) != []int64{active, archived}[i] {
			t.Fatalf("event %v does not carry the new image for its team", p)
		}
	}
}

func TestTeamRegistryInvalidDefaultImageIsRejected(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	for _, img := range []string{
		"", "nginx", "nginx:latest", "registry.example/app:1.2", "repo@", "repo@sha256:", "@sha256:abc",
		"re po@sha256:abc", "repo@@sha256:abc", "repo@sha256@abc", "repo@1sha:abc", "repo@sha256:ab/c",
		"repo@sha256:ab c", "repo@sha-:abc", "repo@sha256:abc\n",
	} {
		if team.ValidDefaultImage(img) {
			t.Fatalf("ValidDefaultImage(%q) = true", img)
		}
		mustCode(t, e, 400, "invalid_request", adminReq("create", nil, map[string]any{"team_name": "beta", "default_image": img}))
		mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": img}))
	}
	if stored, _ := e.repo.Team(id); stored.DefaultImage != imageA || len(e.repo.Teams()) != 1 {
		t.Fatal("an invalid image changed teams")
	}
}

func TestTeamRegistryProviderNeutralDigestAlgorithmsAreAcceptedConsistently(t *testing.T) {
	e := newEnv(t)
	cases := map[string]bool{
		imageA:                                   true,
		imageB:                                   true,
		"x@blake3:abc":                           true,
		"localhost:5000/a/b@sha256+b64u:ab=_-Zz": true,
		"repo@multihash_x.y:Zz09":                true,
		"repo@sha256:":                           false,
		"repo:tag":                               false,
		"repo@9x:abc":                            false,
		"re po@sha256:abc":                       false,
		"repo@sha256:a+b":                        false,
	}
	n := 0
	for img, want := range cases {
		n++
		body, _ := json.Marshal(map[string]any{"params": map[string]any{}, "body": map[string]any{"team_name": "t" + string(rune('a'+n)), "default_image": img}})
		schemaOK := schemas.Validate("admin.create.request.schema.json", body) == nil
		if schemaOK != want || team.ValidDefaultImage(img) != want {
			t.Fatalf("%q: request schema %v, validator %v, want %v", img, schemaOK, team.ValidDefaultImage(img), want)
		}
		rep := e.h.Handle(t.Context(), adminReq("create", nil, map[string]any{"team_name": "t" + string(rune('a'+n)), "default_image": img}))
		if (rep.Status == 201) != want {
			t.Fatalf("%q: handler status %d, want accepted=%v", img, rep.Status, want)
		}
		if want {
			if got := decode(t, rep.Body)["default_image"]; got != img {
				t.Fatalf("%q persisted as %v", img, got)
			}
		}
	}
}
