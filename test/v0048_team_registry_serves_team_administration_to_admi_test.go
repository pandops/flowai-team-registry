//go:build !integration

package test

import (
	"strings"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: team-registry serves team administration to admin-panel over NATS.

func TestTeamRegistryAdministratorReadsOneTeam(t *testing.T) {
	e := newEnv(t)
	active := e.create(t, "alpha", imageA)
	archived := e.create(t, "beta", imageB)
	archivedBody := e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": archived}, nil))
	for _, tc := range []struct {
		id   int64
		name string
		img  string
	}{{active, "alpha", imageA}, {archived, "beta", imageB}} {
		body := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": tc.id}, nil))
		mustValidateBody(t, "admin.get.reply.schema.json", body)
		if len(body) != 5 || asInt(t, body["team_id"]) != tc.id || body["team_name"] != tc.name || body["default_image"] != tc.img {
			t.Fatalf("get %d = %v, want exactly the five canonical fields of %s", tc.id, body, tc.name)
		}
	}
	got := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": archived}, nil))
	if got["archived_at"] != archivedBody["archived_at"] || got["archived_at"] == nil {
		t.Fatalf("archived team read archived_at %v, archive returned %v", got["archived_at"], archivedBody["archived_at"])
	}
}

func TestTeamRegistryRequestedTeamIsMissing(t *testing.T) {
	e := newEnv(t)
	e.create(t, "alpha", imageA)
	before := e.repo.Teams()
	for _, req := range []team.Request{
		adminReq("get", map[string]any{"team_id": 999}, nil),
		adminReq("update", map[string]any{"team_id": 999}, map[string]any{"default_image": imageB}),
		adminReq("archive", map[string]any{"team_id": 999}, nil),
	} {
		body := mustCode(t, e, 404, "canonical_team_not_found", req)
		if asInt(t, body["request_id"]) != requestID {
			t.Fatalf("error body request_id = %v, want %d", body["request_id"], requestID)
		}
	}
	after := e.repo.Teams()
	if len(after) != len(before) || after[0] != before[0] {
		t.Fatalf("a request for an unknown team changed teams: %+v -> %+v", before, after)
	}
}

func TestTeamRegistryNonIntegerTeamIdentifierIsRejected(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	before, _ := e.repo.Team(id)
	rows := len(e.repo.OutboxRows())
	// "1" is the decimal string of an existing id: still not an integer.
	for _, bad := range []any{"9b1e4c02-7d3a-4f8e-b1c6-5a2d9e0f3b71", "acme", 0, -3, 1.5, "1", 9223372036854775808.0, nil, true} {
		for _, req := range []team.Request{
			adminReq("get", map[string]any{"team_id": bad}, nil),
			adminReq("update", map[string]any{"team_id": bad}, map[string]any{"team_name": "zeta"}),
			adminReq("archive", map[string]any{"team_id": bad}, nil),
		} {
			mustCode(t, e, 400, "invalid_request", req)
		}
	}
	if after, _ := e.repo.Team(id); after != before {
		t.Fatalf("team changed by a request with an invalid identifier")
	}
	for _, row := range e.repo.OutboxRows()[rows:] {
		if !strings.HasPrefix(row.Subject, "team-registry.audit.") {
			t.Fatalf("an invalid identifier produced %s", row.Subject)
		}
		if p := decode(t, row.Payload); p["outcome"] != "rejected" || p["team_id"] != nil {
			t.Fatalf("audit of an invalid identifier %v must be rejected and name no team", p)
		}
	}
}

// Scenario: Administrator updates mutable team metadata. v0050: only
// default_image is updatable; team_name is fixed at create. Behaviour matrix
// M3 "update default_image only, active/archived".
func TestTeamRegistryAdministratorUpdatesMutableTeamMetadata(t *testing.T) {
	e := newEnv(t)
	active := e.create(t, "alpha", imageA)
	archived := e.create(t, "beta", imageA)
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": archived}, nil))
	for _, id := range []int64{active, archived} {
		before := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
		published := len(e.publish(t))
		reimaged := e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
		mustPublishOneImageUpdate(t, e, published, id, before["team_name"], imageA, imageB)
		published = len(e.publish(t))
		back := e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageA}))
		mustPublishOneImageUpdate(t, e, published, id, before["team_name"], imageB, imageA)
		for _, got := range []map[string]any{reimaged, back} {
			mustValidateBody(t, "admin.update.reply.schema.json", got)
			if asInt(t, got["team_id"]) != id || got["team_name"] != before["team_name"] ||
				got["ingested_at"] != before["ingested_at"] || got["archived_at"] != before["archived_at"] {
				t.Fatalf("update changed team_id, team_name, ingested_at or archived_at: before %v after %v", before, got)
			}
		}
		if reimaged["default_image"] != imageB || back["default_image"] != imageA {
			t.Fatalf("default_image not stored: %v / %v", reimaged, back)
		}
		stored, _ := e.repo.Team(id)
		if stored.TeamName != before["team_name"] || stored.DefaultImage != imageA {
			t.Fatalf("stored %+v does not match the last update %v", stored, back)
		}
	}
}

// Scenario: Update carrying a team name is rejected. Behaviour matrix M3
// "update with team_name (new, same, with image)", "update team_name with an
// invalid value", "update team_name for an unknown team_id" and
// "update with team_name: null or team_name: 5".
func TestTeamRegistryUpdateCarryingATeamNameIsRejected(t *testing.T) {
	for _, state := range []string{"active", "archived"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			e.repo.SetTeamSeq(2)
			id := e.create(t, "alpha", imageA)
			if id != 3 {
				t.Fatalf("team_id %d, want 3", id)
			}
			if state == "archived" {
				e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
			}
			published := len(e.publish(t))
			before, _ := e.repo.Team(id)
			bodies := []map[string]any{
				{"team_name": "beta"},
				{"team_name": "alpha"},
				{"team_name": "alpha", "default_image": imageB},
				{"team_name": "beta", "default_image": imageB},
				// The name check is not a format check: an invalid string is
				// still immutable, not invalid_request.
				{"team_name": "Beta Team"},
				{"team_name": ""},
			}
			for _, body := range bodies {
				mustCode(t, e, 400, "team_name_immutable", adminReq("update", map[string]any{"team_id": id}, body))
			}
			// Checked before the team is looked up: an unknown team_id gets
			// the same code, not 404.
			mustCode(t, e, 400, "team_name_immutable", adminReq("update", map[string]any{"team_id": 999}, map[string]any{"team_name": "beta"}))
			// A non-string team_name fails the request schema, and so does any
			// other schema violation even when a team_name string is present:
			// an invalid default_image, an unknown field, an identity-provider
			// field.
			schemaViolations := []map[string]any{
				{"team_name": nil},
				{"team_name": 5},
				{"team_name": nil, "default_image": imageB},
				{"team_name": "beta", "default_image": "not-a-digest"},
				{"team_name": "beta", "unknown": 1},
				{"team_name": "beta", "oidc_issuer": "https://idp.example"},
			}
			for _, body := range schemaViolations {
				mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": id}, body))
			}
			// A malformed team_id is still rejected first by the schema.
			mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": "acme"}, map[string]any{"team_name": "beta"}))

			if after, _ := e.repo.Team(id); after != before || after.TeamName != "alpha" || after.DefaultImage != imageA {
				t.Fatalf("a rejected update changed the team: %+v -> %+v", before, after)
			}
			got := e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
			if got["team_name"] != "alpha" || got["default_image"] != imageA {
				t.Fatalf("team after rejected updates = %v, want alpha with the previous image", got)
			}
			msgs := e.publish(t)[published:]
			if events := withPrefix(msgs, "team-registry.event."); len(events) != 0 {
				t.Fatalf("rejected updates published events %v", events)
			}
			audits := bySubject(msgs, "team-registry.audit.team_updated.pub")
			want := len(bodies) + 1 + len(schemaViolations) + 1
			if len(audits) != want {
				t.Fatalf("published %d team_updated audits, want one per rejected update (%d)", len(audits), want)
			}
			for i, m := range audits {
				a := mustValidate(t, m)
				if a["outcome"] != "rejected" || m.Headers["team"] != "admin" || a["actor_subject"] != adminSubject {
					t.Fatalf("audit %d = %v, want outcome rejected by the administrator with team admin", i, a)
				}
				if _, has := a["changes"]; has {
					t.Fatalf("rejected audit %d carries changes %v", i, a["changes"])
				}
				switch {
				case i < len(bodies):
					if a["reason"] != "team_name_immutable" || asInt(t, a["team_id"]) != id {
						t.Fatalf("audit %d = %v, want reason team_name_immutable naming team %d", i, a, id)
					}
				case i == len(bodies):
					if _, has := a["team_id"]; has || a["reason"] != "team_name_immutable" {
						t.Fatalf("audit of the unknown team %v must name no team and carry team_name_immutable", a)
					}
				default:
					if a["reason"] != "invalid_request" {
						t.Fatalf("audit %d = %v, want reason invalid_request", i, a)
					}
				}
			}
		})
	}
}

func TestTeamRegistryUpdateAttemptsImmutableOrForbiddenChanges(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	before, _ := e.repo.Team(id)
	for _, body := range []map[string]any{
		{"team_id": 77},
		{"team_id": 77, "team_name": "beta"},
		{"default_image": ""},
		{"default_image": nil},
		{"team_name": nil},
		{"team_name": "beta", "default_image": nil},
		{"ingested_at": "2026-01-01T00:00:00.000Z"},
		{"archived_at": nil},
		{"team_name": "beta", "oidc_issuer": "https://idp.example"},
		{},
	} {
		mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": id}, body))
	}
	mustCode(t, e, 400, "invalid_request", adminReq("update", map[string]any{"team_id": id}, nil))
	if after, _ := e.repo.Team(id); after != before {
		t.Fatalf("rejected update changed the team: %+v -> %+v", before, after)
	}
}

func TestTeamRegistryAdministratorWriteIsAuditedThroughTheOutbox(t *testing.T) {
	e := newEnv(t)
	e.repo.SetTeamSeq(2)
	id := e.create(t, "alpha", imageA)
	if id != 3 {
		t.Fatalf("team_id %d, want 3", id)
	}
	e.publish(t)
	rowsBefore := len(e.repo.OutboxRows())
	published := len(e.pub.Messages())
	commits := e.repo.Commits
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": 3}, map[string]any{"default_image": imageB}))
	if e.repo.Commits != commits+1 {
		t.Fatalf("update used %d commits, want exactly 1 (change, event and audit in one transaction)", e.repo.Commits-commits)
	}
	newRows := e.repo.OutboxRows()[rowsBefore:]
	subjects := outboxSubjects(newRows)
	if len(newRows) != 2 || !subjectsContain(subjects, "team-registry.event.team_updated.pub") || !subjectsContain(subjects, "team-registry.audit.team_updated.pub") {
		t.Fatalf("outbox rows of the update: %v, want the team_updated event and the team_updated audit", subjects)
	}
	if len(e.pub.Messages()) != published {
		t.Fatal("a message reached the broker before the relay ran: publication is not after commit")
	}
	msgs := e.publish(t)[published:]
	audits := bySubject(msgs, "team-registry.audit.team_updated.pub")
	if len(audits) != 1 {
		t.Fatalf("published %d team_updated audits, want 1", len(audits))
	}
	a := mustValidate(t, audits[0])
	changes, _ := a["changes"].(map[string]any)
	if a["actor_subject"] != adminSubject || a["actor_type"] != "administrator" || a["outcome"] != "succeeded" ||
		asInt(t, a["team_id"]) != 3 || asInt(t, a["request_id"]) != requestID ||
		len(changes) != 2 || changes["old_default_image"] != imageA || changes["new_default_image"] != imageB {
		t.Fatalf("audit payload %v does not carry the administrator, request and exactly the previous and new default_image", a)
	}
	if audits[0].Headers["team"] != "admin" {
		t.Fatalf("audit header team = %q, want admin", audits[0].Headers["team"])
	}
}

func TestTeamRegistryRejectedOrReadAdministratorRequestIsAudited(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	published := len(e.publish(t))
	before, _ := e.repo.Team(id)
	e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
	mustCode(t, e, 404, "canonical_team_not_found", adminReq("update", map[string]any{"team_id": 999}, map[string]any{"default_image": imageB}))
	msgs := e.publish(t)[published:]
	if len(msgs) != 2 {
		t.Fatalf("published %d messages, want exactly two audits", len(msgs))
	}
	read := bySubject(msgs, "team-registry.audit.team_read.pub")
	rejected := bySubject(msgs, "team-registry.audit.team_updated.pub")
	if len(read) != 1 || len(rejected) != 1 {
		t.Fatalf("subjects %v, want one team_read and one team_updated audit", []string{msgs[0].Subject, msgs[1].Subject})
	}
	r := mustValidate(t, read[0])
	if asInt(t, r["team_id"]) != id || r["outcome"] != "succeeded" || read[0].Headers["team"] != "admin" {
		t.Fatalf("read audit %v (team header %q) must name team %d with outcome succeeded and team admin", r, read[0].Headers["team"], id)
	}
	u := mustValidate(t, rejected[0])
	if _, has := u["team_id"]; has || u["resource_id"] != nil || u["outcome"] != "rejected" || u["reason"] != "canonical_team_not_found" || rejected[0].Headers["team"] != "admin" {
		t.Fatalf("rejected update audit %v must carry no team_id, null resource_id, outcome rejected", u)
	}
	if after, _ := e.repo.Team(id); after != before {
		t.Fatal("audited read or rejected update changed a team")
	}
}

// The NATS publish permission on team-registry.admin.*.pub is deployment
// configuration (infrastructure, not tested). team-registry's own part: it
// answers admin subjects only for the administrator context admin-panel sets.
func TestTeamRegistryAdminRequestWithoutAdminContextIsRejected(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, "alpha", imageA)
	rows := len(e.repo.OutboxRows())
	for _, override := range []hdr{{"team": "3"}, {"team": "<delete>"}, {"team": "Admin"}} {
		mustCode(t, e, 400, "invalid_request", adminReq("archive", map[string]any{"team_id": id}, nil, override))
	}
	if stored, _ := e.repo.Team(id); stored.ArchivedAt != nil {
		t.Fatal("an admin request without team: admin archived the team")
	}
	if len(e.repo.OutboxRows()) != rows {
		t.Fatal("a request without the administrator context wrote an outbox row")
	}
	for _, s := range team.Subjects() {
		if !strings.HasPrefix(s, "team-registry.admin.") && s != "team-registry.service.get.sub" {
			t.Fatalf("team-registry serves %s; only team-registry.admin.* and team-registry.service.get are allowed", s)
		}
	}
}

// mustPublishOneImageUpdate checks that a default_image update published,
// after the first `published` messages, exactly one team_updated event with
// the unchanged team_name and the new image, and exactly one team_updated
// audit whose changes are exactly the old and new default_image.
func mustPublishOneImageUpdate(t *testing.T, e *env, published int, id int64, name any, oldImage, newImage string) {
	t.Helper()
	msgs := e.publish(t)[published:]
	events := bySubject(msgs, "team-registry.event.team_updated.pub")
	if len(events) != 1 {
		t.Fatalf("team %d: %d team_updated events, want 1", id, len(events))
	}
	if p := mustValidate(t, events[0]); asInt(t, p["team_id"]) != id || p["team_name"] != name || p["default_image"] != newImage {
		t.Fatalf("team %d: team_updated event %v, want unchanged team_name %v and default_image %s", id, p, name, newImage)
	}
	audits := bySubject(msgs, "team-registry.audit.team_updated.pub")
	if len(audits) != 1 {
		t.Fatalf("team %d: %d team_updated audits, want 1", id, len(audits))
	}
	a := mustValidate(t, audits[0])
	changes, _ := a["changes"].(map[string]any)
	if a["outcome"] != "succeeded" || asInt(t, a["team_id"]) != id ||
		len(changes) != 2 || changes["old_default_image"] != oldImage || changes["new_default_image"] != newImage {
		t.Fatalf("team %d: audit %v, want succeeded with changes exactly old/new default_image %s -> %s", id, a, oldImage, newImage)
	}
}
