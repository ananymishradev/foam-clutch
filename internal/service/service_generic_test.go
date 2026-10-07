package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ananymishradev/foam-clutch/internal/preflight"
	"github.com/ananymishradev/foam-clutch/internal/store"
	"github.com/ananymishradev/foam-clutch/internal/telemetry"
)

// No solver name is hard-coded: foamRun with any physics model
// (incompressibleFluid like heavySimple, fluid like heavyBuoyant) and native
// solvers all share one code path.
func TestGenericSolvers(t *testing.T) {
	cases := []struct {
		name        string
		solver      string
		controlDict string
		wantScript  string
	}{
		{"foamRun-incompressibleFluid", "foamRun",
			"application foamRun;\nsolver incompressibleFluid;\nrunTimeModifiable true;\n",
			"foamRun"},
		{"foamRun-fluid", "foamRun",
			"application foamRun;\nsolver fluid;\nrunTimeModifiable true;\n",
			"foamRun"},
		{"foamRun-customModel", "foamRun",
			"application foamRun;\nsolver myCustomModel;\nrunTimeModifiable true;\n",
			"foamRun"},
		{"native-simpleFoam", "simpleFoam",
			"application simpleFoam;\nrunTimeModifiable true;\n",
			"simpleFoam"},
		{"native-custom", "myCustomSolver123",
			"application myCustomSolver123;\nrunTimeModifiable true;\n",
			"myCustomSolver123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			caseDir := filepath.Join(root, "case")
			writeCase(t, caseDir, tc.controlDict, "2", true)
			mp := filepath.Join(root, "manifest.yaml")
			yml := "apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
				"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
				"solver:\n  name: " + tc.solver + "\n  checkpointSeconds: 60\n" +
				"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
			if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(root, "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			fake := &fakeScheduler{}
			s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
			if _, err := s.Submit(context.Background(), mp); err != nil {
				t.Fatalf("submit %s: %v", tc.solver, err)
			}
			if !contains(fake.spec.Script, tc.wantScript) {
				t.Fatalf("script missing %q", tc.wantScript)
			}
		})
	}
}

func TestControlDictMismatchFailsFast(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\nrunTimeModifiable true;\n", "1", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: pimpleFoam\nstorage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}}
	if _, _, err := s.Validate(context.Background(), mp); err == nil {
		t.Fatal("expected controlDict/solver mismatch error")
	}
}

func TestCheckpointRequiresRunTimeModifiable(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "1", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 30\n" +
		"solver:\n  name: simpleFoam\n  checkpointSeconds: 60\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}}
	if _, _, err := s.Validate(context.Background(), mp); err == nil {
		t.Fatal("expected runTimeModifiable error")
	}
}

func TestSerialRunSkipsDecompose(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\nstorage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	if _, err := s.Submit(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.spec.Script, "serial run, ranks=1") {
		t.Fatal("serial run should skip decomposePar")
	}
	if contains(fake.spec.Script, " -parallel") {
		t.Fatal("serial run must not pass -parallel")
	}
}

func TestSrunAndFileHandlerNone(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n  fileHandler: none\n" +
		"runtime:\n  mpi:\n    launcher: srun\n    flavour: pmix\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	if _, err := s.Submit(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.spec.Script, "srun --mpi=pmix") {
		t.Fatal("expected srun launcher")
	}
	if contains(fake.spec.Script, "-fileHandler") {
		t.Fatal("fileHandler none must omit the flag")
	}
}

func TestCheckpointZeroDisablesSignal(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n  checkpointSeconds: 0\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	if _, err := s.Submit(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if contains(fake.spec.Script, "--signal") || contains(fake.spec.Script, "scontrol requeue") {
		t.Fatal("checkpoint 0 must disable signal/requeue")
	}
	if _, ok := fake.spec.Extra["signal"]; ok {
		t.Fatal("REST extra must not carry signal when checkpoint is 0")
	}
	if fake.spec.Requeue {
		t.Fatal("JobSpec.Requeue must be false when checkpoint is 0")
	}
}
