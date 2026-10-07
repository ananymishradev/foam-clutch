package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	slurmrest "github.com/ananymishradev/foam-clutch"
	"github.com/ananymishradev/foam-clutch/internal/manifest"
	"github.com/ananymishradev/foam-clutch/internal/preflight"
	"github.com/ananymishradev/foam-clutch/internal/stage"
	"github.com/ananymishradev/foam-clutch/internal/store"
	"github.com/ananymishradev/foam-clutch/internal/telemetry"
	"github.com/google/uuid"
)

type Service struct {
	Scheduler slurmrest.Scheduler
	Store     *store.Store
	Preflight preflight.Runner
	Metrics   *telemetry.Metrics
}

type Result struct {
	ID        string
	CaseHash  string
	CachePath string
	RunPath   string
	SlurmID   int
}

func (s *Service) Validate(ctx context.Context, manifestPath string) (manifest.Manifest, preflight.Report, error) {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return manifest.Manifest{}, preflight.Report{}, err
	}
	report, err := s.Preflight.Run(ctx, m.Case.Path)
	if err != nil {
		if s.Metrics != nil {
			s.Metrics.PreflightFailures.Add(1)
		}
		return m, report, err
	}
	return m, report, nil
}

func (s *Service) Submit(ctx context.Context, manifestPath string) (Result, error) {
	if s.Scheduler == nil {
		return Result{}, fmt.Errorf("scheduler is not configured")
	}
	if s.Store == nil {
		return Result{}, fmt.Errorf("store is not configured")
	}
	m, _, err := s.Validate(ctx, manifestPath)
	if err != nil {
		return Result{}, err
	}
	cacheDir := m.Storage.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(filepath.Dir(manifestPath), ".foam-cache")
	}
	stager, err := stage.New(cacheDir)
	if err != nil {
		return Result{}, err
	}
	staged, err := stager.Stage(m.Case.Path)
	if err != nil {
		return Result{}, fmt.Errorf("stage case: %w", err)
	}
	if s.Metrics != nil {
		if staged.Cached {
			s.Metrics.StageCacheHits.Add(1)
		} else {
			s.Metrics.StageCacheMisses.Add(1)
		}
	}
	id := uuid.NewString()
	if err := s.Store.Create(ctx, store.Job{ID: id, Name: m.Name, State: "STAGED", ManifestPath: manifestPath, CaseHash: staged.Hash, UpdatedAt: time.Now()}); err != nil {
		return Result{}, err
	}
	runDir := m.Storage.RunDir
	if runDir == "" {
		runDir = filepath.Join(filepath.Dir(manifestPath), ".foam-runs")
	}
	runPath := filepath.Join(runDir, id)
	if err := stager.Materialize(staged.Path, runPath); err != nil {
		return Result{}, err
	}
	script, err := checkpointScript(m, runPath)
	if err != nil {
		return Result{}, err
	}
	jobID, err := s.Scheduler.Submit(ctx, slurmrest.JobSpec{Name: m.Name, Partition: m.Resources.Partition, Account: m.Resources.Account, Dir: runPath, Nodes: m.Resources.Nodes, TasksPerNode: m.Resources.TasksPerNode, TimeLimitMin: m.Resources.TimeLimitMinutes, Requeue: true, Script: script, Extra: map[string]any{"signal": "B:USR1@600", "exclusive": true, "hint": "nomultithread"}})
	if err != nil {
		_ = s.Store.UpdateState(ctx, id, "FAILED", err.Error(), 0)
		return Result{}, err
	}
	if err := s.Store.UpdateState(ctx, id, "QUEUED", "", jobID); err != nil {
		return Result{}, err
	}
	if s.Metrics != nil {
		s.Metrics.Submissions.Add(1)
	}
	return Result{ID: id, CaseHash: staged.Hash, CachePath: staged.Path, RunPath: runPath, SlurmID: jobID}, nil
}

func checkpointScript(m manifest.Manifest, caseDir string) (string, error) {
	if strings.ContainsAny(caseDir, "\r\n") || strings.ContainsAny(m.Solver.Name, "\r\n") {
		return "", fmt.Errorf("unsafe solver or case path")
	}
	args := make([]string, len(m.Solver.Arguments))
	for i, a := range m.Solver.Arguments {
		args[i] = shellQuote(a)
	}
	solver := shellQuote(m.Solver.Name)
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
cd %s
export OMP_NUM_THREADS=1
checkpoint() {
  foamDictionary -entry stopAt -set writeNow system/controlDict || true
  touch .foam-checkpoint-requested
}
trap checkpoint USR1
if [ -f system/controlDict ]; then
  foamDictionary -entry startFrom -set latestTime system/controlDict || true
fi
srun --mpi=pmix --cpu-bind=cores %s %s -parallel -fileHandler collated > log.solver 2>&1
if [ -f .foam-checkpoint-requested ] && [ -n "${SLURM_JOB_ID:-}" ]; then
  scontrol requeue "$SLURM_JOB_ID"
fi
`, shellQuote(caseDir), solver, strings.Join(args, " ")), nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
