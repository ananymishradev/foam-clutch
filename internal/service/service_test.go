package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	slurmrest "github.com/ananymishradev/foam-clutch"
	"github.com/ananymishradev/foam-clutch/internal/preflight"
	"github.com/ananymishradev/foam-clutch/internal/store"
	"github.com/ananymishradev/foam-clutch/internal/telemetry"
)

type fakeScheduler struct{ spec slurmrest.JobSpec }

func (f *fakeScheduler) Submit(_ context.Context, spec slurmrest.JobSpec) (int, error) {
	f.spec = spec
	return 99, nil
}
func (f *fakeScheduler) State(context.Context, int) ([]string, error) {
	return []string{"RUNNING"}, nil
}
func (f *fakeScheduler) Cancel(context.Context, int) error { return nil }

func TestSubmitStagesPersistsAndBuildsCheckpointScript(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(caseDir, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(caseDir, "system", "controlDict"), []byte("application simpleFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "manifest.yaml")
	data := []byte("apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\nresources:\n  partition: normal\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\nsolver:\n  name: simpleFoam\nstorage:\n  cacheDir: ./cache\n  runDir: ./runs\n")
	if err := os.WriteFile(manifestPath, data, 0o640); err != nil {
		t.Fatal(err)
	}
	// decomposeParDict must agree with 1x1 = 1 rank.
	if err := os.WriteFile(filepath.Join(caseDir, "system", "decomposeParDict"), []byte("numberOfSubdomains 1;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	result, err := s.Submit(context.Background(), manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.SlurmID != 99 || result.CaseHash == "" {
		t.Fatalf("bad result %#v", result)
	}
	if !contains(fake.spec.Script, "scontrol requeue") || !contains(fake.spec.Script, result.RunPath) {
		t.Fatal("checkpoint script was not configured safely")
	}
	if !contains(fake.spec.Script, "blockMesh") || !contains(fake.spec.Script, "decomposePar") {
		t.Fatal("pipeline script must mesh and decompose before the parallel solver")
	}
	if !contains(fake.spec.Script, "#SBATCH --partition=normal") || !contains(fake.spec.Script, "#SBATCH --ntasks=1") {
		t.Fatal("pipeline script must carry SBATCH headers for the cli backend")
	}
	if !contains(fake.spec.Script, "mpirun -np") {
		t.Fatal("pipeline script must launch via mpirun (this cluster's srun pmix plugin is broken)")
	}
	if contains(fake.spec.Script, "srun --mpi=pmix") {
		t.Fatal("pipeline script must not use srun --mpi=pmix here")
	}
	job, err := st.Get(context.Background(), result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "QUEUED" {
		t.Fatalf("state = %s", job.State)
	}
}
func contains(s, part string) bool {
	return len(part) > 0 && len(s) >= len(part) && filepath.Base(part) != "" && strings.Contains(s, part)
}
