package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // Registers the database/sql driver; no C compiler is needed.
)

type sqliteStore struct{ db *sql.DB }

const jobColumns = `id, repository, repository_name, workspace, sha, ref,
 state, message, created_at, finished_at, reported, cleaned, version`

func openSQLiteStore(path string) (*sqliteStore, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path}
	// Apply these settings to every connection, including connections reopened by
	// database/sql. FULL sync makes a successful admission durable before HTTP 202.
	settings := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"}}
	uri.RawQuery = settings.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	closeOnError := func(err error) (*sqliteStore, error) { db.Close(); return nil, err }
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return closeOnError(err)
	}
	if version > 1 {
		return closeOnError(fmt.Errorf("unsupported CI database schema version %d", version))
	}
	_, err = db.Exec(`
 CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  repository TEXT NOT NULL,
  repository_name TEXT NOT NULL,
  workspace TEXT NOT NULL,
  sha TEXT NOT NULL,
  ref TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('queued','running','success','failure','error')),
  message TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0,
  reported INTEGER NOT NULL DEFAULT 0 CHECK (reported IN (0,1)),
  cleaned INTEGER NOT NULL DEFAULT 0 CHECK (cleaned IN (0,1)),
  version INTEGER NOT NULL DEFAULT 1 CHECK (version>0)
 ) STRICT;
 CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(state,created_at,id);
 PRAGMA user_version=1;`)
	if err != nil {
		return closeOnError(err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Admit(ctx context.Context, j job, maxQueued int) (job, bool, error) {
	if maxQueued < 1 || !routeName.MatchString(j.Repository) || !repositoryName.MatchString(j.RepositoryName) || !filepath.IsAbs(j.Workspace) || !commitSHA.MatchString(j.SHA) || j.ID != jobID(j.Repository, j.SHA) || j.State != "queued" || j.CreatedAt.IsZero() {
		return job{}, false, fmt.Errorf("invalid queued job or queue limit")
	}
	// The count and insert are one write statement. Concurrent callers cannot
	// both observe a free queue slot and then overfill it.
	saved, err := scanJob(s.db.QueryRowContext(ctx, `INSERT INTO jobs (`+jobColumns+`)
 SELECT ?,?,?,?,?,?,?,?, ?,0,0,0,1
 WHERE (SELECT count(*) FROM jobs WHERE state='queued') < ?
 ON CONFLICT(id) DO NOTHING RETURNING `+jobColumns,
		j.ID, j.Repository, j.RepositoryName, j.Workspace, j.SHA, j.Ref, j.State, j.Message, j.CreatedAt.UnixNano(), maxQueued))
	if err == nil {
		return saved, true, nil
	}
	if !errors.Is(err, errJobNotFound) {
		return job{}, false, err
	}
	existing, err := s.Get(ctx, j.ID)
	if err == nil {
		return existing, false, nil
	}
	if errors.Is(err, errJobNotFound) {
		return job{}, false, errQueueFull
	}
	return job{}, false, err
}

func (s *sqliteStore) Get(ctx context.Context, id string) (job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
}

func (s *sqliteStore) Claim(ctx context.Context, maxParallel int) (*job, error) {
	if maxParallel < 1 {
		return nil, fmt.Errorf("maxParallel must be positive")
	}
	// SQLite serializes writers. Selection, the running count, and the state
	// change belong to one statement, so the database enforces the limit.
	j, err := scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs
 SET state='running',message='Building and running Dockerfile.chamber',reported=0,version=version+1
 WHERE id=(SELECT id FROM jobs WHERE state='queued' ORDER BY created_at,id LIMIT 1)
 AND (SELECT count(*) FROM jobs WHERE state='running') < ?
 RETURNING `+jobColumns, maxParallel))
	if errors.Is(err, errJobNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *sqliteStore) Finish(ctx context.Context, id, state, message string) error {
	if !(job{State: state}).terminal() {
		return fmt.Errorf("invalid terminal job state %q", state)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs
 SET state=?,message=?,finished_at=?,reported=0,version=version+1 WHERE id=? AND state='running'`, state, message, time.Now().UTC().UnixNano(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return err
	}
	// A lost acknowledgement of a committed write may cause the caller to retry.
	existing, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if existing.State == state && existing.Message == message {
		return nil
	}
	return errJobStateConflict
}

func (s *sqliteStore) Unsettled(ctx context.Context) ([]job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE reported=0 OR cleaned=0 ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *sqliteStore) Acknowledge(ctx context.Context, id string, version int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET reported=1 WHERE id=? AND version=?`, id, version)
	return err
}

func (s *sqliteStore) MarkCleaned(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET cleaned=1 WHERE id=? AND state IN ('success','failure','error')`, id)
	return err
}

func (s *sqliteStore) RecoverInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs
 SET state='error',message='Job interrupted by CI server shutdown or restart',finished_at=?,reported=0,version=version+1
 WHERE state='running'`, time.Now().UTC().UnixNano())
	return err
}

func (s *sqliteStore) RunningCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE state='running'`).Scan(&count)
	return count, err
}

func (s *sqliteStore) CancelQueued(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='error',message='CI server stopped before job started',finished_at=?,reported=0,version=version+1 WHERE state='queued'`, time.Now().UTC().UnixNano())
	return err
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// Both sql.Row and sql.Rows scan the same canonical job columns.
func scanJob(row interface{ Scan(...any) error }) (job, error) {
	var j job
	var created, finished int64
	err := row.Scan(&j.ID, &j.Repository, &j.RepositoryName, &j.Workspace, &j.SHA, &j.Ref, &j.State, &j.Message, &created, &finished, &j.Reported, &j.Cleaned, &j.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return j, errJobNotFound
	}
	if err != nil {
		return j, err
	}
	j.CreatedAt = time.Unix(0, created).UTC()
	if finished != 0 {
		j.FinishedAt = time.Unix(0, finished).UTC()
	}
	return j, nil
}

var _ jobStore = (*sqliteStore)(nil)
