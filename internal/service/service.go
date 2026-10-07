package service

import (
	"context"
	"fmt"
	"os"
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
	if err := checkDecomposition(m); err != nil {
		if s.Metrics != nil {
			s.Metrics.PreflightFailures.Add(1)
		}
		return m, report, err
	}
	if err := checkControlDict(m); err != nil {
		if s.Metrics != nil {
			s.Metrics.PreflightFailures.Add(1)
		}
		return m, report, err
	}
	return m, report, nil
}

// checkDecomposition fails fast when system/decomposeParDict disagrees with
// the requested rank count. A mismatch would otherwise waste queue time and
// then abort inside the job.
func checkDecomposition(m manifest.Manifest) error {
	want := m.Resources.Nodes * m.Resources.TasksPerNode
	got, err := preflight.Subdomains(m.Case.Path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("decomposeParDict numberOfSubdomains=%d does not match resources %d nodes x %d tasks = %d ranks",
			got, m.Resources.Nodes, m.Resources.TasksPerNode, want)
	}
	return nil
}

// checkControlDict ensures the case actually runs the solver named in the
// manifest (foamRun with solver incompressibleFluid, or a native solver
// whose application matches). This catches copy-paste manifests.
func checkControlDict(m manifest.Manifest) error {
	raw, err := os.ReadFile(filepath.Join(m.Case.Path, "system", "controlDict"))
	if err != nil {
		return fmt.Errorf("read system/controlDict: %w", err)
	}
	text := string(raw)
	if m.Solver.Name == "foamRun" {
		if !strings.Contains(text, "application") || !strings.Contains(text, "foamRun") {
			return fmt.Errorf("system/controlDict does not declare application foamRun")
		}
		return nil
	}
	if !strings.Contains(text, m.Solver.Name) {
		return fmt.Errorf("system/controlDict does not reference solver %q", m.Solver.Name)
	}
	return nil
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
	if strings.ContainsAny(m.Name, "\r\n") || strings.ContainsAny(m.Resources.Partition, "\r\n") {
		return "", fmt.Errorf("unsafe job name or partition")
	}
	args := make([]string, len(m.Solver.Arguments))
	for i, a := range m.Solver.Arguments {
		if strings.ContainsAny(a, "\r\n") {
			return "", fmt.Errorf("unsafe solver argument")
		}
		args[i] = shellQuote(a)
	}
	solver := shellQuote(m.Solver.Name)
	ranks := m.Resources.Nodes * m.Resources.TasksPerNode
	wall := wallTime(m.Resources.TimeLimitMinutes)
	accountLine := ""
	if strings.TrimSpace(m.Resources.Account) != "" {
		if strings.ContainsAny(m.Resources.Account, "\r\n") {
			return "", fmt.Errorf("unsafe account")
		}
		accountLine = "#SBATCH --account=" + m.Resources.Account + "\n"
	}
	return fmt.Sprintf(`#!/bin/bash
#SBATCH --job-name=%s
#SBATCH --partition=%s
%s#SBATCH --nodes=%d
#SBATCH --ntasks-per-node=%d
#SBATCH --ntasks=%d
#SBATCH --time=%s
#SBATCH --exclusive
#SBATCH --hint=nomultithread
#SBATCH --requeue
#SBATCH --signal=B:USR1@600
#SBATCH --output=%s/slurm-%%j.out
#SBATCH --error=%s/slurm-%%j.err
set -euo pipefail
echo "=== foam-clutch job: %s ==="
echo "nodes=%d ppn=%d ranks=%d wall=%s"
date; echo "nodelist: ${SLURM_NODELIST:-unknown} job: ${SLURM_JOB_ID:-unknown}"

# OpenFOAM environment (identical install on every node).
# NOTE: foam bashrc dereferences unset $ZSH_NAME, so drop nounset around it.
set +e +u
source %s
set -e -u
echo "OpenFOAM ${WM_PROJECT_VERSION:-unknown} at ${WM_PROJECT_DIR:-unknown}"
which blockMesh decomposePar %s
export OMP_NUM_THREADS=1

cd %s
pwd; ls

# --- mesh (idempotent): skip when polyMesh already exists ---
if [ ! -f constant/polyMesh/points ]; then
  echo "--- blockMesh ---"
  blockMesh 2>&1 | tee log.blockMesh
else
  echo "--- blockMesh skipped (constant/polyMesh exists) ---"
fi

# --- decompose (idempotent): rebuild when subdomain count mismatches ---
echo "--- decomposePar check ---"
NPROC=$(grep -E "^numberOfSubdomains" system/decomposeParDict | grep -o "[0-9]*")
NTASKS=${SLURM_NTASKS:-%d}
echo "numberOfSubdomains=$NPROC NTASKS=$NTASKS"
if [ "$NPROC" != "$NTASKS" ]; then
  echo "ERROR: decomposeParDict($NPROC) != ntasks($NTASKS)" >&2
  exit 1
fi
HAVE_PROC=0
shopt -s nullglob
PROC_DIRS=(processor*)
HAVE_PROC=${#PROC_DIRS[@]}
shopt -u nullglob
if [ "$HAVE_PROC" != "$NTASKS" ]; then
  echo "--- decomposePar ($HAVE_PROC -> $NTASKS) ---"
  rm -rf processor* 2>/dev/null || true
  decomposePar 2>&1 | tee log.decomposePar
else
  echo "--- decomposePar skipped ($HAVE_PROC processor dirs) ---"
fi

# --- solver with USR1 checkpoint/requeue ---
checkpoint() {
  echo "USR1 received: requesting writeNow checkpoint"
  foamDictionary -entry stopAt -set writeNow system/controlDict || true
  touch .foam-checkpoint-requested
}
trap checkpoint USR1
if [ -f system/controlDict ]; then
  foamDictionary -entry startFrom -set latestTime system/controlDict || true
fi
echo "--- mpirun foamRun -parallel (OpenMPI, Slurm-aware) ---"
set +e
mpirun -np "$NTASKS" %s %s -parallel -fileHandler collated > log.solver 2>&1
SOLVER_RC=$?
set -e
echo "solver exit: $SOLVER_RC"
tail -n 20 log.solver || true
if [ -f .foam-checkpoint-requested ] && [ -n "${SLURM_JOB_ID:-}" ]; then
  echo "checkpoint requested: requeue $SLURM_JOB_ID"
  scontrol requeue "$SLURM_JOB_ID"
fi
exit $SOLVER_RC
`, m.Name, m.Resources.Partition, accountLine, m.Resources.Nodes, m.Resources.TasksPerNode, ranks, wall,
		shellQuote(caseDir), shellQuote(caseDir),
		m.Name, m.Resources.Nodes, m.Resources.TasksPerNode, ranks, wall,
		foamBashrc(), m.Solver.Name,
		shellQuote(caseDir), ranks,
		solver, strings.Join(args, " ")), nil
}

// foamBashrc is the host OpenFOAM environment sourced inside every job.
// Override with FOAM_BASHRC when the cluster installs OpenFOAM elsewhere.
func foamBashrc() string {
	if v := strings.TrimSpace(os.Getenv("FOAM_BASHRC")); v != "" {
		return v
	}
	return "/opt/openfoam12/etc/bashrc"
}

func wallTime(minutes int) string {
	if minutes <= 0 {
		minutes = 60
	}
	h := minutes / 60
	m := minutes % 60
	return fmt.Sprintf("%02d:%02d:00", h, m)
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
