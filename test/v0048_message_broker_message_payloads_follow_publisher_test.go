//go:build !integration

package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pandops/flowai-team-registry/schemas"
)

// Requirement: message-broker message payloads follow publisher-owned JSON
// Schemas. "Schema change selects its consumers" is the test runner's
// behavior (qa-tooling, task 1.0): not applicable to team-registry.

func exerciseAllOperations(t *testing.T, e *env) {
	t.Helper()
	id := e.create(t, "alpha", imageA)
	e.mustStatus(t, 200, adminReq("get", map[string]any{"team_id": id}, nil))
	e.mustStatus(t, 200, adminReq("update", map[string]any{"team_id": id}, map[string]any{"default_image": imageB}))
	mustCode(t, e, 400, "team_name_immutable", adminReq("update", map[string]any{"team_id": id}, map[string]any{"team_name": "beta"}))
	e.mustStatus(t, 201, adminReq("archive", map[string]any{"team_id": id}, nil))
	e.mustStatus(t, 200, adminReq("archive", map[string]any{"team_id": id}, nil))
	e.create(t, "gamma", imageA)
	mustCode(t, e, 409, "team_conflict", adminReq("create", nil, map[string]any{"team_name": "gamma", "default_image": imageA}))
	mustCode(t, e, 400, "invalid_request", adminReq("create", nil, map[string]any{"team_name": "a b", "default_image": imageA}))
	mustCode(t, e, 404, "canonical_team_not_found", adminReq("get", map[string]any{"team_id": 404}, nil))
	mustCode(t, e, 404, "canonical_team_not_found", adminReq("archive", map[string]any{"team_id": 404}, nil))
	e.mustStatus(t, 200, serviceGet("1", id))
}

func TestTeamRegistryPublisherMessageViolatesItsOwnSchema(t *testing.T) {
	e := newEnv(t)
	exerciseAllOperations(t, e)
	msgs := e.publish(t)
	kinds := map[string]bool{}
	for _, m := range msgs {
		mustValidate(t, m)
		kinds[m.Subject] = true
	}
	for _, s := range []string{
		"team-registry.event.team_created.pub", "team-registry.event.team_updated.pub", "team-registry.event.team_archived.pub",
		"team-registry.audit.team_created.pub", "team-registry.audit.team_read.pub", "team-registry.audit.team_updated.pub", "team-registry.audit.team_archived.pub",
	} {
		if !kinds[s] {
			t.Fatalf("no message on %s was published", s)
		}
	}
	// A message built without a required field fails its own schema.
	var event, audit map[string]any
	for _, m := range msgs {
		if m.Subject == "team-registry.event.team_created.pub" && event == nil {
			event = decode(t, m.Data)
		}
		if m.Subject == "team-registry.audit.team_updated.pub" && audit == nil {
			audit = decode(t, m.Data)
		}
	}
	delete(event, "team_name")
	bad, _ := json.Marshal(event)
	if schemas.Validate("event.team_created.schema.json", bad) == nil {
		t.Fatal("team_created without team_name passes its schema")
	}
	delete(audit, "actor_subject")
	bad, _ = json.Marshal(audit)
	if schemas.Validate("audit.team_updated.schema.json", bad) == nil {
		t.Fatal("audit without actor_subject passes its schema")
	}
}

// v0050: team names never change, so no audit.team_* schema accepts the
// old_team_name / new_team_name change fields (team-registry serves team
// administration: "the previous and new default_image" only).
func TestTeamRegistryAuditSchemasCarryNoTeamNameChange(t *testing.T) {
	e := newEnv(t)
	exerciseAllOperations(t, e)
	msgs := e.publish(t)
	for _, action := range []string{"team_created", "team_read", "team_updated", "team_archived"} {
		subject := "team-registry.audit." + action + ".pub"
		schema := "audit." + action + ".schema.json"
		sent := bySubject(msgs, subject)
		if len(sent) == 0 {
			t.Fatalf("no message on %s was published", subject)
		}
		base := decode(t, sent[0].Data)
		base["changes"] = map[string]any{"old_default_image": imageA, "new_default_image": imageB}
		ok, _ := json.Marshal(base)
		if err := schemas.Validate(schema, ok); err != nil {
			t.Fatalf("%s rejects a default_image change: %v", schema, err)
		}
		for _, field := range []string{"old_team_name", "new_team_name"} {
			withName := decode(t, sent[0].Data)
			withName["changes"] = map[string]any{"old_default_image": imageA, "new_default_image": imageB, field: "alpha"}
			bad, _ := json.Marshal(withName)
			if schemas.Validate(schema, bad) == nil {
				t.Fatalf("%s accepts changes.%s", schema, field)
			}
		}
	}
	for _, m := range bySubject(msgs, "team-registry.audit.team_updated.pub") {
		p := mustValidate(t, m)
		if changes, _ := p["changes"].(map[string]any); changes["old_team_name"] != nil || changes["new_team_name"] != nil {
			t.Fatalf("team_updated audit %v carries a team-name change", p)
		}
	}
}

// Scenario: Consumer contract test reads the publisher schema. team-registry
// consumes no stream (consumers.json is empty), so it has no consumer
// contract test; its own part is that every subject it publishes and every
// operation it answers has a schema file under svc/team-registry/schemas/
// that consumers can read, and that the files are valid JSON Schemas.
func TestTeamRegistrySchemaFilesAreTheContractConsumersRead(t *testing.T) {
	dir := filepath.Join(serviceDir(t), "schemas")
	consumers, err := os.ReadFile(filepath.Join(dir, "consumers.json"))
	if err != nil || strings.TrimSpace(string(consumers)) != "[]" {
		t.Fatalf("consumers.json = %q (%v), want [] (team-registry consumes no stream)", consumers, err)
	}
	e := newEnv(t)
	exerciseAllOperations(t, e)
	for _, m := range e.publish(t) {
		name, err := schemas.ForSubject(m.Subject)
		if err != nil {
			t.Fatalf("%s: %v", m.Subject, err)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("schema file %s of %s is not on disk: %v", name, m.Subject, err)
		}
	}
	for _, op := range []string{"admin.create", "admin.get", "admin.update", "admin.archive", "service.get"} {
		for _, kind := range []string{"request", "reply"} {
			if _, err := os.Stat(filepath.Join(dir, op+"."+kind+".schema.json")); err != nil {
				t.Fatalf("missing %s.%s.schema.json", op, kind)
			}
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".schema.json") {
			continue
		}
		if err := schemas.Validate(entry.Name(), []byte(`{}`)); err != nil && strings.Contains(err.Error(), "compile") {
			t.Fatalf("%s does not compile: %v", entry.Name(), err)
		}
		if err := schemas.Validate(entry.Name(), []byte(`{}`)); err == nil {
			t.Fatalf("%s accepts an empty object: it constrains nothing", entry.Name())
		}
	}
}
