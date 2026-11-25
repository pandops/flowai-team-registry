//go:build !integration

package test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/mocks"
	"github.com/pandops/flowai-team-registry/internal/outbox"
)

// Requirement: service-platform service gets foreign data only from the
// broker. team-registry owns teams and reads no foreign data; its part of
// "Team archive reaches a projection" is that the archive event alone lets a
// consumer keep a projection. "Projection ignores a replayed event" is the
// consumers' behavior; team-registry's part is that a republished message
// keeps its publisher_seq and Nats-Msg-Id so the replay is recognizable.

func TestTeamRegistryTeamArchiveReachesAProjection(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	id := e.create(t, "alpha", imageA)
	// A consumer's projection, fed only by published events.
	projection := map[int64]map[string]any{}
	applied := map[string]bool{}
	e.pub.OnPublish = func(m mocks.Message) {
		if !applied[m.MsgID] && len(m.Subject) > len("team-registry.event.") && m.Subject[:len("team-registry.event.")] == "team-registry.event." {
			applied[m.MsgID] = true
			var p map[string]any
			_ = json.Unmarshal(m.Data, &p)
			projection[int64(p["team_id"].(float64))] = p
		}
	}
	e.publish(t)
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	e.publish(t)
	got := projection[3]
	if got == nil || got["kind"] != "team_archived" || got["archived_at"] == nil || got["team_name"] != "alpha" {
		t.Fatalf("projection of team 3 after archive = %v", got)
	}
}

// failMark fails the first MarkOutboxSent: the message was acknowledged by
// JetStream but the service stopped before recording it.
type failMark struct {
	outbox.Store
	failed bool
}

func (f *failMark) MarkOutboxSent(ctx context.Context, seq int64) error {
	if !f.failed {
		f.failed = true
		return errors.New("stopped before marking")
	}
	return f.Store.MarkOutboxSent(ctx, seq)
}

func TestTeamRegistryRepublishAfterAckKeepsItsIdentity(t *testing.T) {
	repo := mocks.NewMemRepo()
	pub := &mocks.Publisher{}
	store := &failMark{Store: repo}
	e := newEnvWith(repo, store, pub)
	e.create(t, "alpha", imageA)
	if _, err := e.relay.RunOnce(context.Background()); err == nil {
		t.Fatal("relay reported success although marking failed")
	}
	restarted := outbox.NewRelay(repo, pub, quietLogger(), 0)
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := pub.Messages()
	if len(msgs) < 3 || msgs[0].MsgID != msgs[1].MsgID {
		t.Fatalf("the unmarked message was not republished with the same Nats-Msg-Id: %+v", msgs)
	}
	a, b := decode(t, msgs[0].Data), decode(t, msgs[1].Data)
	if asInt(t, a["publisher_seq"]) != asInt(t, b["publisher_seq"]) || string(msgs[0].Data) != string(msgs[1].Data) {
		t.Fatal("republished message differs from the original")
	}
	for _, r := range repo.OutboxRows() {
		if !repo.Sent(r.Seq) {
			t.Fatalf("row %d not marked sent after the restarted relay ran", r.Seq)
		}
	}
}
