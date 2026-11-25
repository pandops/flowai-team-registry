//go:build !integration

package test

import (
	"strings"
	"testing"
)

// Requirement: message-broker never carries secrets in streams or logs —
// Stream content inspection (team-registry's own messages).

func TestTeamRegistryStreamContentInspection(t *testing.T) {
	e := newEnv(t)
	_, adminJWT := writeCreds(t, t.TempDir())
	secretHeaders := hdr{
		"Authorization": "Bearer " + adminJWT,
		"admin_token":   adminJWT,
		"Cookie":        "session=s3cr3t-cookie",
	}
	id := e.create(t, "alpha", imageA)
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}, secretHeaders))
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil, secretHeaders))
	e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil, secretHeaders))
	mustCode(t, e, 404, "canonical_team_not_found", adminReq("get", map[string]any{"team_id": 99}, nil, secretHeaders))
	msgs := e.publish(t)
	if len(msgs) == 0 {
		t.Fatal("nothing published")
	}
	for _, m := range msgs {
		blob := string(m.Data)
		for k, v := range m.Headers {
			blob += "\n" + k + ": " + v
		}
		for _, secret := range []string{adminJWT, "eyJ", "Bearer", "s3cr3t-cookie", "seed", "SUA"} {
			if strings.Contains(blob, secret) {
				t.Fatalf("%s carries %q", m.Subject, secret)
			}
		}
		for k := range m.Headers {
			if k != "team" && k != "request_id" {
				t.Fatalf("%s carries header %q; only team and request_id are allowed", m.Subject, k)
			}
		}
	}
}
