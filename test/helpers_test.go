// Package test holds team-registry unit tests (default build) and
// PostgreSQL integration tests (build tag integration). Unit tests use only
// team-registry packages, the standard library and the in-process fakes in
// internal/mocks.
package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pandops/flowai-team-registry/internal/mocks"
	"github.com/pandops/flowai-team-registry/internal/outbox"
	"github.com/pandops/flowai-team-registry/internal/team"
	"github.com/pandops/flowai-team-registry/schemas"
)

const (
	adminSubject = "admin-subject-7f3c"
	requestID    = int64(1001)
	digest64     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	imageA       = "registry.example/flowai/runtime@sha256:" + digest64
	imageB       = "ghcr.io/flowai/runtime:1.2@sha512:" + digest64 + digest64
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// env is one team-registry handler over the in-memory repository, with the
// outbox relay publishing to the fake JetStream publisher.
type env struct {
	repo  *mocks.MemRepo
	pub   *mocks.Publisher
	h     *team.Handler
	relay *outbox.Relay
}

func newEnvWith(repo team.Repository, store outbox.Store, pub *mocks.Publisher) *env {
	e := &env{pub: pub}
	if m, ok := repo.(*mocks.MemRepo); ok {
		e.repo = m
	}
	e.h = team.NewHandler(repo, func() time.Time { return time.Now().UTC() }, quietLogger())
	e.relay = outbox.NewRelay(store, pub, quietLogger(), 10*time.Millisecond)
	return e
}

func newEnv(t *testing.T) *env {
	t.Helper()
	repo := mocks.NewMemRepo()
	return newEnvWith(repo, repo, &mocks.Publisher{})
}

type hdr map[string]string

func adminHeaders() hdr {
	return hdr{"team": "admin", "request_id": strconv.FormatInt(requestID, 10), "admin_subject": adminSubject}
}

// envelope builds the agents.md rule 9 request body.
func envelope(params map[string]any, body any) []byte {
	m := map[string]any{}
	if params == nil {
		params = map[string]any{}
	}
	m["params"] = params
	if body != nil {
		m["body"] = body
	}
	data, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return data
}

func adminReq(op string, params map[string]any, body any, override ...hdr) team.Request {
	h := adminHeaders()
	for _, o := range override {
		for k, v := range o {
			if v == "<delete>" {
				delete(h, k)
			} else {
				h[k] = v
			}
		}
	}
	return team.Request{Subject: "team-registry.admin." + op + ".sub", Header: h, Data: envelope(params, body)}
}

func serviceGet(teamHeader string, teamID any) team.Request {
	h := hdr{"request_id": strconv.FormatInt(requestID, 10)}
	if teamHeader != "" {
		h["team"] = teamHeader
	}
	return team.Request{Subject: "team-registry.service.get.sub", Header: h, Data: envelope(map[string]any{"team_id": teamID}, nil)}
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return m
}

func (e *env) do(t *testing.T, req team.Request) (int, map[string]any) {
	t.Helper()
	rep := e.h.Handle(context.Background(), req)
	if len(rep.Body) == 0 {
		return rep.Status, nil
	}
	return rep.Status, decode(t, rep.Body)
}

// mustStatus runs req and fails unless the reply has status want.
func (e *env) mustStatus(t *testing.T, want int, req team.Request) map[string]any {
	t.Helper()
	status, body := e.do(t, req)
	if status != want {
		t.Fatalf("%s: status %d, want %d; body %v", req.Subject, status, want, body)
	}
	return body
}

func (e *env) create(t *testing.T, name, image string) int64 {
	t.Helper()
	body := e.mustStatus(t, 201, adminReq("create", nil, map[string]any{"team_name": name, "default_image": image}))
	return asInt(t, body["team_id"])
}

func asInt(t *testing.T, v any) int64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("value %#v is not a JSON number", v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("value %s is not an int64: %v", n, err)
	}
	return i
}

// publish runs the relay until nothing is pending and returns every message
// the fake publisher acknowledged so far.
func (e *env) publish(t *testing.T) []mocks.Message {
	t.Helper()
	if _, err := e.relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("relay: %v", err)
	}
	return e.pub.Messages()
}

func errorCode(body map[string]any) string {
	code, _ := body["code"].(string)
	return code
}

func mustCode(t *testing.T, e *env, wantStatus int, wantCode string, req team.Request) map[string]any {
	t.Helper()
	body := e.mustStatus(t, wantStatus, req)
	if errorCode(body) != wantCode {
		t.Fatalf("%s: code %q, want %q; body %v", req.Subject, errorCode(body), wantCode, body)
	}
	mustValidateBody(t, "error.reply.schema.json", body)
	return body
}

func mustValidateBody(t *testing.T, schema string, body map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(schema, data); err != nil {
		t.Fatalf("body %s does not validate against %s: %v", data, schema, err)
	}
}

// mustValidate checks a published message against its publisher schema.
func mustValidate(t *testing.T, m mocks.Message) map[string]any {
	t.Helper()
	name, err := schemas.ForSubject(m.Subject)
	if err != nil {
		t.Fatalf("no schema for %s: %v", m.Subject, err)
	}
	if err := schemas.Validate(name, m.Data); err != nil {
		t.Fatalf("message on %s does not validate against %s: %v; %s", m.Subject, name, err, m.Data)
	}
	return decode(t, m.Data)
}

func bySubject(msgs []mocks.Message, subject string) []mocks.Message {
	var out []mocks.Message
	for _, m := range msgs {
		if m.Subject == subject {
			out = append(out, m)
		}
	}
	return out
}

func withPrefix(msgs []mocks.Message, prefix string) []mocks.Message {
	var out []mocks.Message
	for _, m := range msgs {
		if strings.HasPrefix(m.Subject, prefix) {
			out = append(out, m)
		}
	}
	return out
}

func outboxSubjects(rows []outbox.Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Subject)
	}
	return out
}

func subjectsContain(list []string, needle string) bool {
	for _, s := range list {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// serviceDir returns the team-registry module root.
func serviceDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller unknown")
	}
	return filepath.Dir(filepath.Dir(file))
}

// configFixture writes a complete, valid configuration with its credential
// and CA files into a temp dir and returns the YAML text and the dir.
func configFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"db-ca.pem", "nats-ca.pem", "user.creds"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	yaml := fmt.Sprintf(`http:
  listen: ":8080"
db:
  host: postgres.flowai.svc
  port: 5432
  user: team_registry
  password: db-secret-value
  name: flowai
  sslmode: verify-full
  ca_file: %[1]s/db-ca.pem
nats:
  url: tls://nats.flowai.svc:4222
  credentials_file: %[1]s/user.creds
  ca_file: %[1]s/nats-ca.pem
  inbox_prefix: _INBOX.team-registry
  request_timeout: 5s
`, dir)
	return yaml, dir
}
