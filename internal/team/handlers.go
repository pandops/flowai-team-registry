package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"

	"github.com/pandops/flowai-team-registry/schemas"
)

type op struct {
	name    string // operation token of the subject
	action  string // audit action
	request string // request schema file
}

var (
	opCreate  = op{"create", "team_created", "admin.create.request.schema.json"}
	opGet     = op{"get", "team_read", "admin.get.request.schema.json"}
	opUpdate  = op{"update", "team_updated", "admin.update.request.schema.json"}
	opArchive = op{"archive", "team_archived", "admin.archive.request.schema.json"}
)

var positive = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// parsePositive parses a positive decimal int64 with no sign, padding or
// exponent.
func parsePositive(s string) (int64, bool) {
	if !positive.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// envelope is the rule 9 request body after schema validation.
type envelope struct {
	Params struct {
		TeamID json.Number `json:"team_id"`
	} `json:"params"`
	Body struct {
		TeamName     *string `json:"team_name"`
		DefaultImage *string `json:"default_image"`
	} `json:"body"`
}

// decodeRequest validates data against the operation's own request schema
// (closed objects, integer range, name and image rules) and decodes it. The
// schema is the single definition of what a valid request is.
func decodeRequest(schema string, data []byte) (envelope, int64, bool) {
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	decodeErr := dec.Decode(&env)
	// The id is kept even for an invalid body, so a rejection audit can name
	// an existing team.
	// json.Number also accepts a quoted string such as "1"; only a bare JSON
	// number is an identifier.
	var raw struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(data, &raw)
	id, idOK := int64(0), false
	if tok := raw.Params["team_id"]; len(tok) > 0 && tok[0] >= '0' && tok[0] <= '9' {
		id, idOK = parsePositive(string(tok))
	}
	if err := schemas.Validate(schema, data); err != nil || decodeErr != nil {
		return env, id, false
	}
	if env.Params.TeamID == "" {
		return env, 0, true
	}
	return env, id, idOK
}

// rejection is a client error that is audited and answered, after the
// transaction that found it rolled back.
type rejection struct {
	status int
	code   string
	msg    string
}

func (r *rejection) Error() string { return r.code }

func reject(status int, code, msg string) error { return &rejection{status, code, msg} }

func (h *Handler) admin(ctx context.Context, req Request, requestID int64, o op) Reply {
	actor := req.Header["admin_subject"]
	if req.Header["team"] != "admin" || actor == "" || len(actor) > 512 {
		// Without the administrator context there is no actor to audit.
		return errorReply(400, "invalid_request", "administrator context is required", requestID)
	}
	a := audit{action: o.action, actor: actor, requestID: requestID}
	env, id, ok := decodeRequest(o.request, req.Data)
	if !ok {
		return h.rejected(ctx, a, id, reject(400, "invalid_request", "request does not match "+o.request))
	}
	if o == opUpdate && env.Body.TeamName != nil {
		// Team names never change after create; the rejection comes before
		// the team is looked up and is audited like any other.
		return h.rejected(ctx, a, id, reject(400, "team_name_immutable", "team_name cannot be changed"))
	}
	var (
		status int
		result Team
	)
	err := h.repo.WithTx(ctx, func(tx Tx) error {
		m := messages{ctx: ctx, tx: tx, now: h.now().UTC(), requestID: requestID}
		var err error
		switch o {
		case opCreate:
			result, err = tx.InsertTeam(ctx, *env.Body.TeamName, *env.Body.DefaultImage)
			if errors.Is(err, ErrNameConflict) {
				return reject(409, "team_conflict", "team_name is taken")
			}
			if err != nil {
				return err
			}
			status = 201
			if err := m.event("team_created", result); err != nil {
				return err
			}
			return m.audit(a.succeeded(result.TeamID, map[string]any{"team_name": result.TeamName, "default_image": result.DefaultImage}))
		case opGet:
			if result, err = tx.GetTeam(ctx, id); err != nil {
				return notFound(err)
			}
			status = 200
			return m.audit(a.succeeded(id, nil))
		case opUpdate:
			before, err := tx.LockTeam(ctx, id)
			if err != nil {
				return notFound(err)
			}
			image := ""
			if env.Body.DefaultImage != nil {
				image = *env.Body.DefaultImage
			}
			result, err = tx.UpdateTeam(ctx, id, image)
			if err != nil {
				return err
			}
			status = 200
			if err := m.event("team_updated", result); err != nil {
				return err
			}
			return m.audit(a.succeeded(id, map[string]any{
				"old_default_image": before.DefaultImage, "new_default_image": result.DefaultImage,
			}))
		case opArchive:
			before, err := tx.LockTeam(ctx, id)
			if err != nil {
				return notFound(err)
			}
			if before.ArchivedAt != nil {
				// Idempotent retry: same representation, no second event.
				result, status = before, 200
				return m.audit(a.succeeded(id, map[string]any{"archived_at": formatTime(*before.ArchivedAt)}))
			}
			if result, err = tx.ArchiveTeam(ctx, id); err != nil {
				return err
			}
			status = 201
			if err := m.event("team_archived", result); err != nil {
				return err
			}
			return m.audit(a.succeeded(id, map[string]any{"archived_at": formatTime(*result.ArchivedAt)}))
		}
		return errors.New("unknown operation")
	})
	var rej *rejection
	if errors.As(err, &rej) {
		return h.rejected(ctx, a, id, rej)
	}
	if err != nil {
		h.logger.Error("team-registry request failed", "operation", o.name, "request_id", requestID, "error", err.Error())
		return errorReply(500, "internal_error", "internal error", requestID)
	}
	return teamReply(status, result)
}

func notFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return reject(404, "canonical_team_not_found", "team not found")
	}
	return err
}

// rejected writes the rejected audit in its own transaction and answers the
// client error. The audit names the team only when it exists. If the audit
// cannot be committed the request fails with 500 and nothing is published.
func (h *Handler) rejected(ctx context.Context, a audit, id int64, rej error) Reply {
	var r *rejection
	if !errors.As(rej, &r) {
		h.logger.Error("team-registry rejection without a status", "action", a.action, "request_id", a.requestID)
		return errorReply(500, "internal_error", "internal error", a.requestID)
	}
	err := h.repo.WithTx(ctx, func(tx Tx) error {
		var teamID int64
		if id > 0 {
			if _, err := tx.GetTeam(ctx, id); err == nil {
				teamID = id
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		m := messages{ctx: ctx, tx: tx, now: h.now().UTC(), requestID: a.requestID}
		return m.audit(a.rejected(teamID, r.code))
	})
	if err != nil {
		h.logger.Error("team-registry rejection audit failed", "action", a.action, "request_id", a.requestID, "error", err.Error())
		return errorReply(500, "internal_error", "internal error", a.requestID)
	}
	return errorReply(r.status, r.code, r.msg, a.requestID)
}

// serviceGet is the point read for source-controller, executor-registry and
// API Gateway. The team header must name the requested team or be admin. It
// is not audited.
func (h *Handler) serviceGet(ctx context.Context, req Request, requestID int64) Reply {
	_, id, ok := decodeRequest("service.get.request.schema.json", req.Data)
	if !ok {
		return errorReply(400, "invalid_request", "request does not match service.get.request.schema.json", requestID)
	}
	if hdr := req.Header["team"]; hdr != "admin" && hdr != strconv.FormatInt(id, 10) {
		return errorReply(400, "invalid_request", "team header must be the requested team_id or admin", requestID)
	}
	var result Team
	err := h.repo.WithTx(ctx, func(tx Tx) error {
		var err error
		result, err = tx.GetTeam(ctx, id)
		return notFound(err)
	})
	var rej *rejection
	if errors.As(err, &rej) {
		return errorReply(rej.status, rej.code, rej.msg, requestID)
	}
	if err != nil {
		h.logger.Error("team point read failed", "request_id", requestID, "error", err.Error())
		return errorReply(500, "internal_error", "internal error", requestID)
	}
	return teamReply(200, result)
}
