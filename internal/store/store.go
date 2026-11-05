// Package store keeps the local state: which object is published under which
// slug, the hash of the last output, and the assets it carries.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("store: not found")

type Entry struct {
	ObjectID    string
	SpaceID     string
	Slug        string
	Mode        string
	Title       string
	ContentHash string
	// Assets maps a file object id to the asset file name under a/.
	Assets map[string]string
	// SourceModified is the last_modified_date of the row the output was built from.
	SourceModified string
	// Version is the directory name under the version store the slug points at.
	Version string
	// Active is false for a parked slug kept for keep_slug_on_republish.
	Active      bool
	PublishedAt time.Time
	UpdatedAt   time.Time
	// RenderVersion invalidates outputs built by an older renderer.
	RenderVersion int
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create state dir: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w (state.path %s)", err, path)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS objects (
	object_id       TEXT PRIMARY KEY,
	space_id        TEXT NOT NULL,
	slug            TEXT NOT NULL UNIQUE,
	mode            TEXT NOT NULL DEFAULT 'slug',
	zk_key          BLOB,
	title           TEXT NOT NULL DEFAULT '',
	content_hash    TEXT NOT NULL DEFAULT '',
	asset_ids       TEXT NOT NULL DEFAULT '{}',
	source_modified TEXT NOT NULL DEFAULT '',
	version         TEXT NOT NULL DEFAULT '',
	active          INTEGER NOT NULL DEFAULT 1,
	render_version  INTEGER NOT NULL DEFAULT 0,
	published_at    TIMESTAMP NOT NULL,
	updated_at      TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS objects_space ON objects(space_id);`)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

const cols = `object_id, space_id, slug, mode, title, content_hash, asset_ids, source_modified, version, active, render_version, published_at, updated_at`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (Entry, error) {
	var e Entry
	var assets string
	var active int
	err := r.Scan(&e.ObjectID, &e.SpaceID, &e.Slug, &e.Mode, &e.Title, &e.ContentHash, &assets, &e.SourceModified, &e.Version, &active, &e.RenderVersion, &e.PublishedAt, &e.UpdatedAt)
	if err != nil {
		return e, err
	}
	e.Active = active == 1
	e.Assets = map[string]string{}
	_ = json.Unmarshal([]byte(assets), &e.Assets)
	return e, nil
}

func (s *Store) Get(ctx context.Context, objectID string) (Entry, error) {
	e, err := scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM objects WHERE object_id = ?`, objectID))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) SlugExists(ctx context.Context, slug string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE slug = ?`, slug).Scan(&n)
	return n > 0, err
}

// List returns all entries, optionally restricted to one space ("" = all).
func (s *Store) List(ctx context.Context, spaceID string) ([]Entry, error) {
	q := `SELECT ` + cols + ` FROM objects`
	var args []any
	if spaceID != "" {
		q += ` WHERE space_id = ?`
		args = append(args, spaceID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Put(ctx context.Context, e Entry) error {
	assets, err := json.Marshal(e.Assets)
	if err != nil {
		return err
	}
	if e.Mode == "" {
		e.Mode = "slug"
	}
	now := time.Now().UTC()
	if e.PublishedAt.IsZero() {
		e.PublishedAt = now
	}
	e.UpdatedAt = now
	active := 0
	if e.Active {
		active = 1
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO objects (`+cols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(object_id) DO UPDATE SET
	space_id=excluded.space_id, slug=excluded.slug, mode=excluded.mode, title=excluded.title,
	content_hash=excluded.content_hash, asset_ids=excluded.asset_ids,
	source_modified=excluded.source_modified, version=excluded.version, active=excluded.active,
	render_version=excluded.render_version, published_at=excluded.published_at, updated_at=excluded.updated_at`,
		e.ObjectID, e.SpaceID, e.Slug, e.Mode, e.Title, e.ContentHash, string(assets), e.SourceModified, e.Version, active, e.RenderVersion, e.PublishedAt, e.UpdatedAt)
	return err
}

func (s *Store) Delete(ctx context.Context, objectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM objects WHERE object_id = ?`, objectID)
	return err
}

func (s *Store) CountActive(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE active = 1`).Scan(&n)
	return n, err
}
