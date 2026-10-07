package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Job struct {
	ID           string
	Name         string
	State        string
	ManifestPath string
	CaseHash     string
	SlurmID      int
	Error        string
	UpdatedAt    time.Time
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL,
 manifest_path TEXT NOT NULL, case_hash TEXT NOT NULL, slurm_id INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL
); CREATE INDEX IF NOT EXISTS jobs_updated_at ON jobs(updated_at);`); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Create(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,name,state,manifest_path,case_hash,slurm_id,error,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		j.ID, j.Name, j.State, j.ManifestPath, j.CaseHash, j.SlurmID, j.Error, j.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) UpdateState(ctx context.Context, id, state, message string, slurmID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?,error=?,slurm_id=CASE WHEN ? > 0 THEN ? ELSE slurm_id END,updated_at=? WHERE id=?`,
		state, message, slurmID, slurmID, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	var j Job
	var ts string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,state,manifest_path,case_hash,slurm_id,error,updated_at FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Name, &j.State, &j.ManifestPath, &j.CaseHash, &j.SlurmID, &j.Error, &ts)
	if err != nil {
		return Job{}, err
	}
	j.UpdatedAt, err = time.Parse(time.RFC3339Nano, ts)
	return j, err
}
func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,state,manifest_path,case_hash,slurm_id,error,updated_at FROM jobs ORDER BY updated_at DESC LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Job
	for rows.Next() {
		var j Job
		var ts string
		if err := rows.Scan(&j.ID, &j.Name, &j.State, &j.ManifestPath, &j.CaseHash, &j.SlurmID, &j.Error, &ts); err != nil {
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
func (s *Store) String() string { return fmt.Sprintf("store(%p)", s) }
