//go:build !integration

package test

import (
	"testing"
)

// Requirement: team-registry publishes team changes for downstream projections.

func TestTeamRegistryConsumersReceiveTeamChangesInOrder(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageA}))
	events := withPrefix(e.publish(t), "team-registry.event.")
	if len(events) != 3 {
		t.Fatalf("%d events, want created + two updates", len(events))
	}
	// Projection of a downstream consumer: keyed by team_id, deduplicated by
	// (publisher, publisher_seq), applied in arrival order.
	type projected struct{ name, image string }
	projection := map[int64]projected{}
	seen := map[int64]bool{}
	var lastSeq int64
	for i, ev := range events {
		p := mustValidate(t, ev)
		seq := asInt(t, p["publisher_seq"])
		if seq <= lastSeq {
			t.Fatalf("event %d has publisher_seq %d after %d: not in commit order", i, seq, lastSeq)
		}
		lastSeq = seq
		if seen[seq] {
			continue
		}
		seen[seq] = true
		if p["team_name"] != "alpha" {
			t.Fatalf("event %d carries team_name %v, want the unchanged alpha", i, p["team_name"])
		}
		projection[asInt(t, p["team_id"])] = projected{name: p["team_name"].(string), image: p["default_image"].(string)}
	}
	if events[1].Subject != "team-registry.event.team_updated.pub" || events[2].Subject != "team-registry.event.team_updated.pub" {
		t.Fatalf("subjects %s, %s, want two team_updated", events[1].Subject, events[2].Subject)
	}
	if p1, p2 := mustValidate(t, events[1]), mustValidate(t, events[2]); p1["default_image"] != imageB || p2["default_image"] != imageA {
		t.Fatalf("team_updated events carry %v then %v, want %s then %s", p1["default_image"], p2["default_image"], imageB, imageA)
	}
	if projection[id].name != "alpha" || projection[id].image != imageA {
		t.Fatalf("projection ends with %+v, want alpha with the second default_image", projection[id])
	}
}
