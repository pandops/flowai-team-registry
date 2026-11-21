// Package store is the PostgreSQL repository of team-registry.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pandops/flowai-team-registry/internal/outbox"
	"github.com/pandops/flowai-team-registry/internal/team"
)

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Store implements team.Repository and outbox.Store in one schema.
type Store struct {
	db     *sql.DB
	schema string
}

// New returns a Store working only in schema.
func New(db *sql.DB, schema string) (*Store, error) {
	if !ident.MatchString(schema) {
		return nil, fmt.Errorf("store: invalid schema name %q", schema)
	}
	return &Store{db: db, schema: schema}, nil
}

func (s *Store) t(name string) string { return s.schema + "." + name }

// WithTx runs fn in one transaction and returns fn's error unchanged after
// rolling back.
func (s *Store) WithTx(ctx context.Context, fn func(team.Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&tx{s: s, tx: sqlTx}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

type tx struct {
	s  *Store
	tx *sql.Tx
}

const columns = `team_id, team_name, default_image, ingested_at, archived_at`

func scan(row *sql.Row) (team.Team, error) {
	var (
		t        team.Team
		archived sql.NullTime
	)
	err := row.Scan(&t.TeamID, &t.TeamName, &t.DefaultImage, &t.IngestedAt, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return team.Team{}, team.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return team.Team{}, team.ErrNameConflict
	}
	if err != nil {
		return team.Team{}, err
	}
	t.IngestedAt = t.IngestedAt.UTC().Truncate(time.Millisecond)
	if archived.Valid {
		a := archived.Time.UTC().Truncate(time.Millisecond)
		t.ArchivedAt = &a
	}
	return t, nil
}

func (x *tx) InsertTeam(ctx context.Context, name, image string) (team.Team, error) {
	return scan(x.tx.QueryRowContext(ctx, `INSERT INTO `+x.s.t("teams")+` (team_name, default_image) VALUES ($1, $2) RETURNING `+columns, name, image))
}

func (x *tx) GetTeam(ctx context.Context, id int64) (team.Team, error) {
	return scan(x.tx.QueryRowContext(ctx, `SELECT `+columns+` FROM `+x.s.t("teams")+` WHERE team_id = $1`, id))
}

func (x *tx) LockTeam(ctx context.Context, id int64) (team.Team, error) {
	return scan(x.tx.QueryRowContext(ctx, `SELECT `+columns+` FROM `+x.s.t("teams")+` WHERE team_id = $1 FOR UPDATE`, id))
}

func (x *tx) UpdateTeam(ctx context.Context, id int64, image string) (team.Team, error) {
	return scan(x.tx.QueryRowContext(ctx, `UPDATE `+x.s.t("teams")+`
		SET default_image = COALESCE(NULLIF($2, ''), default_image),
		    updated_at = now()
		WHERE team_id = $1 RETURNING `+columns, id, image))
}

func (x *tx) ArchiveTeam(ctx context.Context, id int64) (team.Team, error) {
	return scan(x.tx.QueryRowContext(ctx, `UPDATE `+x.s.t("teams")+`
		SET archived_at = COALESCE(archived_at, now()), updated_at = now()
		WHERE team_id = $1 RETURNING `+columns, id))
}

func (x *tx) NextPublisherSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := x.tx.QueryRowContext(ctx, `SELECT nextval('`+x.s.t("publisher_seq")+`')`).Scan(&seq)
	return seq, err
}

func (x *tx) InsertOutbox(ctx context.Context, row outbox.Row) error {
	headers, err := json.Marshal(row.Headers)
	if err != nil {
		return err
	}
	_, err = x.tx.ExecContext(ctx, `INSERT INTO `+x.s.t("outbox")+` (publisher_seq, subject, headers, payload) VALUES ($1, $2, $3, $4)`,
		row.Seq, row.Subject, headers, row.Payload)
	return err
}

// PendingOutbox returns unsent rows in publisher_seq order.
func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]outbox.Row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT publisher_seq, subject, headers, payload FROM `+s.t("outbox")+`
		WHERE sent_at IS NULL ORDER BY publisher_seq LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outbox.Row
	for rows.Next() {
		var (
			r       outbox.Row
			headers []byte
		)
		if err := rows.Scan(&r.Seq, &r.Subject, &headers, &r.Payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(headers, &r.Headers); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkOutboxSent marks one row sent.
func (s *Store) MarkOutboxSent(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE `+s.t("outbox")+` SET sent_at = now() WHERE publisher_seq = $1 AND sent_at IS NULL`, seq)
	return err
}

// Ping checks the database.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
