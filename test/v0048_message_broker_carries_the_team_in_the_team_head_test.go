//go:build !integration

package test

import (
	"strconv"
	"testing"
)

// Requirement: message-broker carries the team in the team header —
// Administrator action carries team admin (team-registry's half; admin-panel
// tests its request headers).

func TestTeamRegistryAdministratorActionCarriesTeamAdmin(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	if id := e.create(t, "alpha", imageA); id != 3 {
		t.Fatalf("team_id %d, want 3", id)
	}
	published := len(e.publish(t))
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": 3}, map[string]any{"default_image": imageB}, hdr{"request_id": "777"}))
	msgs := e.publish(t)[published:]
	if len(msgs) != 2 {
		t.Fatalf("%d messages for the update, want event and audit", len(msgs))
	}
	for _, m := range msgs {
		p := mustValidate(t, m)
		if m.Headers["team"] != "admin" {
			t.Fatalf("%s header team = %q, want admin", m.Subject, m.Headers["team"])
		}
		if m.Headers["request_id"] != "777" {
			t.Fatalf("%s header request_id = %q, want the request's 777", m.Subject, m.Headers["request_id"])
		}
		if asInt(t, p["team_id"]) != 3 {
			t.Fatalf("%s payload team_id = %v, want 3", m.Subject, p["team_id"])
		}
		if m.MsgID != "team-registry-"+strconv.FormatInt(asInt(t, p["publisher_seq"]), 10) {
			t.Fatalf("%s Nats-Msg-Id %q does not match publisher_seq %v", m.Subject, m.MsgID, p["publisher_seq"])
		}
	}
}
