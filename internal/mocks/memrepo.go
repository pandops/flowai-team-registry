// Package mocks holds in-process fakes of team-registry's own interfaces for
// unit tests. Production wiring never imports it.
package mocks

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pandops/flowai-team-registry/internal/outbox"
	"github.com/pandops/flowai-team-registry/internal/team"
)

// ErrUnavailable is returned while the fake database is marked down.
var ErrUnavailable = errors.New("fake database unavailable")

// MemRepo is an in-memory team.Repository and outbox.Store. Transactions are
// serialized and copy-on-write: an error from fn discards every change except
// sequence numbers, which, like PostgreSQL sequences, are never given back.
type MemRepo struct {
	mu         sync.Mutex
	teams      map[int64]team.Team
	outbox     map[int64]outbox.Row
	sent       map[int64]bool
	teamSeq    int64
	pubSeq     int64
	down       bool
	Now        func() time.Time
	Commits    int
	Rollbacks  int
	failCommit error
}

// NewMemRepo returns an empty repository.
func NewMemRepo() *MemRepo {
	return &MemRepo{
		teams:  map[int64]team.Team{},
		outbox: map[int64]outbox.Row{},
		sent:   map[int64]bool{},
		Now:    func() time.Time { return time.Now().UTC() },
	}
}

// SetDown marks the database unavailable (true) or available (false).
func (m *MemRepo) SetDown(down bool) { m.mu.Lock(); m.down = down; m.mu.Unlock() }

// FailNextCommit makes the next commit fail with err (the transaction rolls back).
func (m *MemRepo) FailNextCommit(err error) { m.mu.Lock(); m.failCommit = err; m.mu.Unlock() }

// SetTeamSeq sets the last assigned team_id (as if earlier teams existed).
func (m *MemRepo) SetTeamSeq(v int64) { m.mu.Lock(); m.teamSeq = v; m.mu.Unlock() }

// Team returns the stored team.
func (m *MemRepo) Team(id int64) (team.Team, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.teams[id]
	return t, ok
}

// Teams returns every stored team ordered by team_id.
func (m *MemRepo) Teams() []team.Team {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]team.Team, 0, len(m.teams))
	for _, t := range m.teams {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TeamID < out[j].TeamID })
	return out
}

// OutboxRows returns every committed outbox row ordered by publisher_seq.
func (m *MemRepo) OutboxRows() []outbox.Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]outbox.Row, 0, len(m.outbox))
	for _, r := range m.outbox {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// WithTx implements team.Repository.
func (m *MemRepo) WithTx(ctx context.Context, fn func(team.Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	tx := &memTx{repo: m, teams: map[int64]team.Team{}, outbox: map[int64]outbox.Row{}}
	for k, v := range m.teams {
		tx.teams[k] = v
	}
	if err := fn(tx); err != nil {
		m.Rollbacks++
		return err
	}
	if m.failCommit != nil {
		err := m.failCommit
		m.failCommit = nil
		m.Rollbacks++
		return err
	}
	m.teams = tx.teams
	for k, v := range tx.outbox {
		m.outbox[k] = v
	}
	m.Commits++
	return nil
}

// PendingOutbox implements outbox.Store.
func (m *MemRepo) PendingOutbox(ctx context.Context, limit int) ([]outbox.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, ErrUnavailable
	}
	var out []outbox.Row
	for seq, r := range m.outbox {
		if !m.sent[seq] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkOutboxSent implements outbox.Store.
func (m *MemRepo) MarkOutboxSent(ctx context.Context, seq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	m.sent[seq] = true
	return nil
}

// Sent reports whether a row was marked sent.
func (m *MemRepo) Sent(seq int64) bool { m.mu.Lock(); defer m.mu.Unlock(); return m.sent[seq] }

type memTx struct {
	repo   *MemRepo
	teams  map[int64]team.Team
	outbox map[int64]outbox.Row
}

func (t *memTx) nameTaken(name string, except int64) bool {
	for id, other := range t.teams {
		if id != except && strings.EqualFold(other.TeamName, name) {
			return true
		}
	}
	return false
}

func (t *memTx) InsertTeam(ctx context.Context, name, image string) (team.Team, error) {
	t.repo.teamSeq++ // a sequence value is consumed even when the insert fails
	id := t.repo.teamSeq
	if t.nameTaken(name, 0) {
		return team.Team{}, team.ErrNameConflict
	}
	row := team.Team{TeamID: id, TeamName: name, DefaultImage: image, IngestedAt: t.repo.Now().Truncate(time.Millisecond)}
	t.teams[id] = row
	return row, nil
}

func (t *memTx) GetTeam(ctx context.Context, id int64) (team.Team, error) {
	row, ok := t.teams[id]
	if !ok {
		return team.Team{}, team.ErrNotFound
	}
	return row, nil
}

func (t *memTx) LockTeam(ctx context.Context, id int64) (team.Team, error) { return t.GetTeam(ctx, id) }

func (t *memTx) UpdateTeam(ctx context.Context, id int64, image string) (team.Team, error) {
	row, ok := t.teams[id]
	if !ok {
		return team.Team{}, team.ErrNotFound
	}
	if image != "" {
		row.DefaultImage = image
	}
	t.teams[id] = row
	return row, nil
}

func (t *memTx) ArchiveTeam(ctx context.Context, id int64) (team.Team, error) {
	row, ok := t.teams[id]
	if !ok {
		return team.Team{}, team.ErrNotFound
	}
	if row.ArchivedAt == nil {
		at := t.repo.Now().Truncate(time.Millisecond)
		row.ArchivedAt = &at
	}
	t.teams[id] = row
	return row, nil
}

func (t *memTx) NextPublisherSeq(ctx context.Context) (int64, error) {
	t.repo.pubSeq++
	return t.repo.pubSeq, nil
}

func (t *memTx) InsertOutbox(ctx context.Context, row outbox.Row) error {
	if _, dup := t.outbox[row.Seq]; dup {
		return errors.New("duplicate publisher_seq")
	}
	if _, dup := t.repo.outbox[row.Seq]; dup {
		return errors.New("duplicate publisher_seq")
	}
	t.outbox[row.Seq] = row
	return nil
}
