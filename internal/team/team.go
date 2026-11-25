// Package team holds the team-registry domain: validation, the request
// handlers for the admin and service subjects, and the messages they
// publish through the outbox.
package team

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/pandops/flowai-team-registry/internal/outbox"
)

// Team is one canonical team.
type Team struct {
	TeamID       int64
	TeamName     string
	DefaultImage string
	IngestedAt   time.Time
	ArchivedAt   *time.Time
}

var (
	// ErrNotFound reports an unknown team.
	ErrNotFound = errors.New("team not found")
	// ErrNameConflict reports a team_name taken ignoring case.
	ErrNameConflict = errors.New("team name conflict")
)

// Tx is one repository transaction.
type Tx interface {
	InsertTeam(ctx context.Context, name, image string) (Team, error)
	GetTeam(ctx context.Context, id int64) (Team, error)
	// LockTeam reads the team and holds its row until the transaction ends,
	// so one team's changes take publisher_seq values in commit order.
	LockTeam(ctx context.Context, id int64) (Team, error)
	// UpdateTeam changes default_image when image is non-empty; team names
	// never change after create.
	UpdateTeam(ctx context.Context, id int64, image string) (Team, error)
	// ArchiveTeam sets archived_at only when it is not set yet.
	ArchiveTeam(ctx context.Context, id int64) (Team, error)
	NextPublisherSeq(ctx context.Context) (int64, error)
	InsertOutbox(ctx context.Context, row outbox.Row) error
}

// Repository runs transactions; fn's error rolls the transaction back.
type Repository interface {
	WithTx(ctx context.Context, fn func(Tx) error) error
}

var (
	teamName     = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	defaultImage = regexp.MustCompile(`^[^\s@]+@[A-Za-z][A-Za-z0-9]*(?:[+._-][A-Za-z][A-Za-z0-9]*)*:[A-Za-z0-9=_-]+$`)
)

// ValidTeamName reports whether name is a valid team name: 1 to 10
// characters, lowercase ASCII letters and digits in hyphen-separated groups,
// starting with a letter. It mirrors admin.create.request.schema.json.
func ValidTeamName(name string) bool {
	return len(name) <= 10 && teamName.MatchString(name)
}

// ValidDefaultImage reports whether image is a digest-bearing OCI reference.
func ValidDefaultImage(image string) bool { return defaultImage.MatchString(image) }

// QueueGroup is the NATS queue group of every subscription.
const QueueGroup = "team-registry"

const (
	subjCreate  = "team-registry.admin.create.sub"
	subjGet     = "team-registry.admin.get.sub"
	subjUpdate  = "team-registry.admin.update.sub"
	subjArchive = "team-registry.admin.archive.sub"
	subjService = "team-registry.service.get.sub"
)

// Subjects returns the subjects team-registry serves.
func Subjects() []string {
	return []string{subjCreate, subjGet, subjUpdate, subjArchive, subjService}
}

// Request is one NATS request.
type Request struct {
	Subject string
	Header  map[string]string
	Data    []byte
}

// Reply is the answer: status header and body.
type Reply struct {
	Status int
	Header map[string]string
	Body   []byte
}

// Handler serves team-registry requests.
type Handler struct {
	repo   Repository
	now    func() time.Time
	logger *slog.Logger
}

// NewHandler returns a handler.
func NewHandler(repo Repository, now func() time.Time, logger *slog.Logger) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{repo: repo, now: now, logger: logger}
}

// Handle answers one request.
func (h *Handler) Handle(ctx context.Context, req Request) Reply {
	requestID, ok := parsePositive(req.Header["request_id"])
	if !ok {
		return errorReply(400, "invalid_request", "request_id header must be a positive decimal integer", 0)
	}
	switch req.Subject {
	case subjService:
		return h.serviceGet(ctx, req, requestID)
	case subjCreate:
		return h.admin(ctx, req, requestID, opCreate)
	case subjGet:
		return h.admin(ctx, req, requestID, opGet)
	case subjUpdate:
		return h.admin(ctx, req, requestID, opUpdate)
	case subjArchive:
		return h.admin(ctx, req, requestID, opArchive)
	}
	return errorReply(400, "invalid_request", "unknown operation", requestID)
}
