package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Job is the local record for one submission. RunPath and CachePath locate
// the isolated working copy and the content-addressed input it was cloned
// from; older databases without those columns read back as "".
type Job struct {
	ID           string
	Name         string
	State        string
	ManifestPath string
	CaseHash     string
	RunPath      string
	CachePath    string
	SlurmID      int
	Error        string
	UpdatedAt    time.Time
}

// States is the local job state machine. QUEUED/RUNNING/REQUEUED/UNKNOWN are
// live; DONE/FAILED/CANCELLED are terminal.
var ValidStates = map[string]bool{
	"STAGED": true, "QUEUED": true, "RUNNING": true, "REQUEUED": true,
	"UNKNOWN": true, "DONE": true, "FAILED": true, "CANCELLED": true,
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL,
 manifest_path TEXT NOT NULL, case_hash TEXT NOT NULL, slurm_id INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL
); CREATE INDEX IF NOT EXISTS jobs_updated_at ON jobs(updated_at);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state);`); err != nil {
		db.Close()
		return nil, err
	}
	// Additive migrations for databases created before run/cache paths were
	// persisted. ALTER ... ADD COLUMN is idempotent via the probe below.
	for _, col := range []string{"run_path", "cache_path"} {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='` + col + `'`).Scan(&n)
		if err != nil {
			db.Close()
			return nil, err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Create(ctx context.Context, j Job) error {
	if j.ID == "" {
		return fmt.Errorf("job ID is required")
	}
	if !ValidStates[j.State] {
		return fmt.Errorf("invalid job state %q", j.State)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,name,state,manifest_path,case_hash,run_path,cache_path,slurm_id,error,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Name, j.State, j.ManifestPath, j.CaseHash, j.RunPath, j.CachePath, j.SlurmID, j.Error, j.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) UpdateState(ctx context.Context, id, state, message string, slurmID int) error {
	if !ValidStates[state] {
		return fmt.Errorf("invalid job state %q", state)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?,error=?,slurm_id=CASE WHEN ? > 0 THEN ? ELSE slurm_id END,updated_at=? WHERE id=?`,
		state, message, slurmID, slurmID, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// UpdatePaths records the materialized run/cache locations after staging.
func (s *Store) UpdatePaths(ctx context.Context, id, runPath, cachePath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET run_path=?,cache_path=?,updated_at=? WHERE id=?`,
		runPath, cachePath, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	var j Job
	var ts string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,state,manifest_path,case_hash,run_path,cache_path,slurm_id,error,updated_at FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Name, &j.State, &j.ManifestPath, &j.CaseHash, &j.RunPath, &j.CachePath, &j.SlurmID, &j.Error, &ts)
	if err != nil {
		return Job{}, err
	}
	j.UpdatedAt, err = time.Parse(time.RFC3339Nano, ts)
	return j, err
}
func (s *Store) List(ctx context.Context) ([]Job, error) {
	return s.ListByState(ctx, "")
}

// ListByState filters by local state; empty state lists all (newest first).
func (s *Store) ListByState(ctx context.Context, state string) ([]Job, error) {
	q := `SELECT id,name,state,manifest_path,case_hash,run_path,cache_path,slurm_id,error,updated_at FROM jobs ORDER BY updated_at DESC LIMIT 1000`
	var rows *sql.Rows
	var err error
	if state != "" {
		q = `SELECT id,name,state,manifest_path,case_hash,run_path,cache_path,slurm_id,error,updated_at FROM jobs WHERE state=? ORDER BY updated_at DESC LIMIT 1000`
		rows, err = s.db.QueryContext(ctx, q, state)
	} else {
		rows, err = s.db.QueryContext(ctx, q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Job
	for rows.Next() {
		var j Job
		var ts string
		if err := rows.Scan(&j.ID, &j.Name, &j.State, &j.ManifestPath, &j.CaseHash, &j.RunPath, &j.CachePath, &j.SlurmID, &j.Error, &ts); err != nil {
			return nil, err
		}
		j.UpdatedAt, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, err
		}
		result = append(result, j)
	}
	return result, rows.Err()
}

// GetBySlurm finds the local job for a Slurm ID (newest wins on requeue).
func (s *Store) GetBySlurm(ctx context.Context, slurmID int) (Job, error) {
	var j Job
	var ts string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,state,manifest_path,case_hash,run_path,cache_path,slurm_id,error,updated_at FROM jobs WHERE slurm_id=? ORDER BY updated_at DESC LIMIT 1`, slurmID).Scan(&j.ID, &j.Name, &j.State, &j.ManifestPath, &j.CaseHash, &j.RunPath, &j.CachePath, &j.SlurmID, &j.Error, &ts)
	if err != nil {
		return Job{}, err
	}
	j.UpdatedAt, err = time.Parse(time.RFC3339Nano, ts)
	return j, err
}
func (s *Store) String() string { return fmt.Sprintf("store(%p)", s) }
