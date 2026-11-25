package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/pandops/flowai-team-registry/internal/outbox"
	"github.com/pandops/flowai-team-registry/schemas"
)

const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func representation(t Team) map[string]any {
	var archived any
	if t.ArchivedAt != nil {
		archived = formatTime(*t.ArchivedAt)
	}
	return map[string]any{
		"team_id":       t.TeamID,
		"team_name":     t.TeamName,
		"default_image": t.DefaultImage,
		"ingested_at":   formatTime(t.IngestedAt),
		"archived_at":   archived,
	}
}

func teamReply(status int, t Team) Reply {
	body, _ := json.Marshal(representation(t))
	return Reply{Status: status, Body: body}
}

func errorReply(status int, code, message string, requestID int64) Reply {
	m := map[string]any{"code": code, "message": message}
	if requestID > 0 {
		m["request_id"] = requestID
	}
	body, _ := json.Marshal(m)
	return Reply{Status: status, Body: body}
}

// audit is the fixed audit field set of one administrator request.
type audit struct {
	action    string
	actor     string
	requestID int64
}

func (a audit) payload(teamID int64, outcome, reason string, changes map[string]any) map[string]any {
	p := map[string]any{
		"source_service": "team-registry",
		"actor_type":     "administrator",
		"actor_subject":  a.actor,
		"action":         a.action,
		"resource_type":  "team",
		"resource_id":    nil,
		"request_id":     a.requestID,
		"outcome":        outcome,
	}
	if teamID > 0 {
		p["team_id"] = teamID
		p["resource_id"] = teamID
	}
	if reason != "" {
		p["reason"] = reason
	}
	if len(changes) > 0 {
		p["changes"] = changes
	}
	return p
}

func (a audit) succeeded(teamID int64, changes map[string]any) map[string]any {
	return a.payload(teamID, "succeeded", "", changes)
}

func (a audit) rejected(teamID int64, reason string) map[string]any {
	return a.payload(teamID, "rejected", reason, nil)
}

// messages writes outbox rows inside one transaction.
type messages struct {
	ctx       context.Context
	tx        Tx
	now       time.Time
	requestID int64
}

func (m messages) event(kind string, t Team) error {
	p := representation(t)
	p["kind"] = kind
	return m.write("team-registry.event."+kind+".pub", "event."+kind+".schema.json", p)
}

func (m messages) audit(p map[string]any) error {
	action := p["action"].(string)
	return m.write("team-registry.audit."+action+".pub", "audit."+action+".schema.json", p)
}

// write adds the stream envelope, checks the payload against the
// publisher's own schema and stores the row; publication happens only after
// commit, through the relay.
func (m messages) write(subject, schema string, p map[string]any) error {
	seq, err := m.tx.NextPublisherSeq(m.ctx)
	if err != nil {
		return err
	}
	p["publisher"] = "team-registry"
	p["publisher_seq"] = seq
	p["occurred_at"] = formatTime(m.now)
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := schemas.Validate(schema, data); err != nil {
		return fmt.Errorf("%s payload violates %s: %w", subject, schema, err)
	}
	return m.tx.InsertOutbox(m.ctx, outbox.Row{
		Seq:     seq,
		Subject: subject,
		Headers: map[string]string{"team": "admin", "request_id": strconv.FormatInt(m.requestID, 10)},
		Payload: data,
	})
}
