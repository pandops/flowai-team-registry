//go:build !integration

package test

import (
	"encoding/json"
	"testing"
)

// Requirement: service-platform service returns non-revealing not-found for
// foreign resources. Teams have no owning team, so the scenarios about a
// foreign task or environment do not apply; team-registry's part is that a
// missing, never-created and archived-then-missing id all get one 404 shape
// that names no team.

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTeamRegistryNotFoundHasOneShape(t *testing.T) {
	e := newEnv(t)
	e.create(t, "alpha", imageA)
	shape := func(body map[string]any) string {
		delete(body, "request_id")
		return string(mustJSON(t, body))
	}
	var want string
	for _, req := range []struct {
		name string
		id   int64
		op   string
	}{
		{"get 2", 2, "get"}, {"get max", 9223372036854775807, "get"},
		{"update 2", 2, "update"}, {"archive 2", 2, "archive"},
	} {
		var body map[string]any
		if req.op == "update" {
			body = mustCode(t, e, 404, "canonical_team_not_found", adminReq(req.op, map[string]any{"team_id": req.id}, map[string]any{"default_image": imageB}))
		} else {
			body = mustCode(t, e, 404, "canonical_team_not_found", adminReq(req.op, map[string]any{"team_id": req.id}, nil))
		}
		if body["request_id"] == nil {
			t.Fatalf("%s: no request_id", req.name)
		}
		s := shape(body)
		if want == "" {
			want = s
		} else if s != want {
			t.Fatalf("%s: not-found body %s differs from %s", req.name, s, want)
		}
	}
	svc := mustCode(t, e, 404, "canonical_team_not_found", serviceGet("admin", 2))
	if s := shape(svc); s != want {
		t.Fatalf("service.get not-found %s differs from %s", s, want)
	}
	for _, leak := range []string{"alpha", imageA} {
		if contains(want, leak) {
			t.Fatalf("not-found body reveals %q", leak)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
