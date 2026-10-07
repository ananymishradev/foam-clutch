package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	slurmrest "github.com/ananymishradev/foam-clutch"
	"github.com/ananymishradev/foam-clutch/internal/manifest"
	"github.com/ananymishradev/foam-clutch/internal/preflight"
	schedcli "github.com/ananymishradev/foam-clutch/internal/scheduler"
	"github.com/ananymishradev/foam-clutch/internal/service"
	"github.com/ananymishradev/foam-clutch/internal/store"
	"github.com/ananymishradev/foam-clutch/internal/supervisor"
	"github.com/ananymishradev/foam-clutch/internal/telemetry"
)

// Version is set at build time: go build -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// `clutch -manifest m.yaml` (leading flag, no subcommand) defaults to run.
	if strings.HasPrefix(os.Args[1], "-") {
		if err := run(ctx, os.Args[1:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = validate(ctx, os.Args[2:])
	case "submit":
		err = submit(ctx, os.Args[2:])
	case "run", "it":
		err = run(ctx, os.Args[2:])
	case "watch":
		err = watch(ctx, os.Args[2:])
	case "status":
		err = status(ctx, os.Args[2:])
	case "cancel":
		err = cancel(ctx, os.Args[2:])
	case "list":
		err = listJobs(ctx, os.Args[2:])
	case "logs":
		err = logs(ctx, os.Args[2:])
	case "init":
		err = initManifest(os.Args[2:])
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "version":
		fmt.Printf("clutch %s\n", Version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func submit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "path to manifest")
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	watch := fs.Bool("watch", false, "watch Slurm state until the job reaches a terminal state")
	backend := fs.String("backend", "auto", "scheduler backend: auto, rest, cli")
	timeout := fs.Duration("timeout", 5*time.Minute, "submit timeout (0 disables)")
	interval := fs.Duration("interval", 15*time.Second, "watch poll interval when -watch is set")
	_ = fs.Parse(args)
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	scheduler, backendName, err := selectScheduler(*backend)
	if err != nil {
		return err
	}
	log.Printf("clutch submit backend=%s manifest=%s", backendName, *manifestPath)
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	s := service.Service{Scheduler: scheduler, Store: st, Preflight: preflight.Runner{}, Metrics: metrics}
	// Submit uses a bounded timeout; the optional watch below inherits the
	// parent signal context so Ctrl-C stops polling without losing the job
	// record (Slurm keeps running; re-watch later by local ID).
	result, err := s.Submit(ctx, *manifestPath)
	if err != nil {
		return err
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if *watch {
		wctx := context.WithoutCancel(ctx)
		wctx, stop := signal.NotifyContext(wctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		w := supervisor.Supervisor{Scheduler: scheduler, Store: st, Metrics: metrics, PollInterval: *interval}
		if err := w.Watch(wctx, result.ID, result.SlurmID); err != nil {
			return err
		}
	}
	return nil
}

// run is the single command that validates, submits, and watches an
// OpenFOAM case until Slurm reaches a terminal state:
//
//	clutch run -manifest /home/shared/openfoam/heavySimple/manifest.yaml
//
// Defaults are chosen so that running from the case directory needs no flags
// at all (manifest.yaml + foam-clutch.db next to it, CLI backend). It is
// equivalent to validate + submit -watch, with an optional live log tail.
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "path to manifest")
	dbPath := fs.String("db", "", "SQLite database path (default <manifest-dir>/foam-clutch.db)")
	backend := fs.String("backend", "auto", "scheduler backend: auto, rest, cli")
	timeout := fs.Duration("timeout", 5*time.Minute, "submit-phase timeout (0 disables)")
	interval := fs.Duration("interval", 15*time.Second, "watch poll interval (e.g. 10s, 1m)")
	tail := fs.Bool("tail", false, "stream log.solver and slurm-*.out while watching")
	tailInterval := fs.Duration("tail-interval", 5*time.Second, "log tail poll interval")
	_ = fs.Parse(args)

	db := strings.TrimSpace(*dbPath)
	if db == "" {
		db = filepath.Join(filepath.Dir(*manifestPath), "foam-clutch.db")
	}
	submitCtx := ctx
	if *timeout > 0 {
		var cancel context.CancelFunc
		submitCtx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	scheduler, backendName, err := selectScheduler(*backend)
	if err != nil {
		return err
	}
	log.Printf("clutch run backend=%s manifest=%s db=%s", backendName, *manifestPath, db)
	// Fail fast on manifest/case problems before touching the database or
	// Slurm, so a typo never creates stray state.
	vs := service.Service{Preflight: preflight.Runner{}}
	if _, _, err := vs.Validate(ctx, *manifestPath); err != nil {
		return err
	}
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	s := service.Service{Scheduler: scheduler, Store: st, Preflight: preflight.Runner{}, Metrics: metrics}
	result, err := s.Submit(submitCtx, *manifestPath)
	if err != nil {
		return err
	}
	log.Printf("submitted id=%s slurm=%d run=%s", result.ID, result.SlurmID, result.RunPath)
	log.Printf("solver log: %s", filepath.Join(result.RunPath, "log.solver"))
	log.Printf("slurm log:  %s", filepath.Join(result.RunPath, fmt.Sprintf("slurm-%d.out", result.SlurmID)))
	_ = json.NewEncoder(os.Stdout).Encode(result)

	// Watching is decoupled from the submit timeout: Ctrl-C stops polling
	// without losing the job record (Slurm keeps running; re-watch later).
	wctx := context.WithoutCancel(ctx)
	wctx, stop := signal.NotifyContext(wctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *tail {
		go tailRunLogs(wctx, result.RunPath, result.SlurmID, *tailInterval)
	}
	w := supervisor.Supervisor{Scheduler: scheduler, Store: st, Metrics: metrics, PollInterval: *interval}
	if err := w.Watch(wctx, result.ID, result.SlurmID); err != nil {
		return err
	}
	j, err := st.Get(wctx, result.ID)
	if err != nil {
		return err
	}
	log.Printf("terminal state=%s slurm=%d", j.State, j.SlurmID)
	return json.NewEncoder(os.Stdout).Encode(j)
}

// tailRunLogs streams new lines from the solver log and the Slurm output
// and error files until ctx is done. Missing files are tolerated (they
// appear once the job starts); offsets reset if a file shrinks.
func tailRunLogs(ctx context.Context, runPath string, slurmID int, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	files := []struct {
		label string
		path  string
	}{{"solver", filepath.Join(runPath, "log.solver")},
		{"slurm-out", filepath.Join(runPath, fmt.Sprintf("slurm-%d.out", slurmID))},
		{"slurm-err", filepath.Join(runPath, fmt.Sprintf("slurm-%d.err", slurmID))}}
	offsets := make([]int64, len(files))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i, f := range files {
				n, err := tailFrom(f.path, offsets[i], f.label)
				if err == nil {
					offsets[i] = n
				}
			}
		}
	}
}

func tailFrom(path string, offset int64, label string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if st.Size() < offset {
		offset = 0
	}
	if st.Size() == offset {
		return offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return offset, err
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		fmt.Printf("[%s] %s", label, line)
		if !strings.HasSuffix(line, "\n") {
			fmt.Println()
		}
	}
	return offset + int64(len(data)), nil
}

// selectScheduler picks the Slurm backend. "auto" prefers slurmrestd when the
// REST env is fully set and falls back to sbatch/squeue CLI otherwise, which
// is the common case on clusters without slurmrestd.
//
// Token handling is production-minded: SLURM_JWT_FILE (refreshed on every
// request, e.g. `scontrol token lifespan=... > file`) wins over the static
// SLURM_JWT value, and SLURM_SOCKET selects Unix-socket transport.
func selectScheduler(backend string) (slurmrest.Scheduler, string, error) {
	baseURL := os.Getenv("SLURM_REST_URL")
	apiVersion := os.Getenv("SLURM_API_VERSION")
	user := os.Getenv("SLURM_USER")
	socket := os.Getenv("SLURM_SOCKET")
	tokenFn := tokenProvider()
	_, hasStatic := os.LookupEnv("SLURM_JWT")
	_, hasFile := os.LookupEnv("SLURM_JWT_FILE")
	restReady := apiVersion != "" && user != "" && (baseURL != "" || socket != "") && (hasStatic || hasFile)
	switch backend {
	case "rest":
		if !restReady {
			return nil, "", fmt.Errorf("backend=rest requires SLURM_API_VERSION, SLURM_USER, SLURM_JWT or SLURM_JWT_FILE, and SLURM_REST_URL or SLURM_SOCKET")
		}
		c, err := slurmrest.NewClient(slurmrest.Config{
			BaseURL: baseURL, SocketPath: socket, APIVersion: apiVersion, User: user,
			Token: tokenFn,
		})
		if err != nil {
			return nil, "", err
		}
		return c, "rest", nil
	case "cli":
		return &schedcli.CliScheduler{}, "cli", nil
	case "auto", "":
		if restReady {
			c, err := slurmrest.NewClient(slurmrest.Config{
				BaseURL: baseURL, SocketPath: socket, APIVersion: apiVersion, User: user,
				Token: tokenFn,
			})
			if err != nil {
				return nil, "", err
			}
			return c, "rest", nil
		}
		return &schedcli.CliScheduler{}, "cli", nil
	default:
		return nil, "", fmt.Errorf("unknown backend %q (want auto, rest, or cli)", backend)
	}
}

// tokenProvider returns a refreshable JWT supplier. The file is re-read on
// every request so rotated tokens (scontrol token) are picked up without a
// restart; the static env value is the fallback for short-lived setups.
func tokenProvider() func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		if p := strings.TrimSpace(os.Getenv("SLURM_JWT_FILE")); p != "" {
			raw, err := os.ReadFile(p)
			if err != nil {
				return "", fmt.Errorf("read SLURM_JWT_FILE: %w", err)
			}
			if tok := strings.TrimSpace(string(raw)); tok != "" {
				return tok, nil
			}
			return "", fmt.Errorf("SLURM_JWT_FILE %q is empty", p)
		}
		if tok := strings.TrimSpace(os.Getenv("SLURM_JWT")); tok != "" {
			return tok, nil
		}
		return "", fmt.Errorf("no Slurm token: set SLURM_JWT_FILE or SLURM_JWT")
	}
}

func validate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "path to manifest")
	_ = fs.Parse(args)
	s := service.Service{Preflight: preflight.Runner{}}
	m, report, err := s.Validate(ctx, *manifestPath)
	out := struct {
		Manifest any    `json:"manifest"`
		Report   any    `json:"report"`
		Error    string `json:"error,omitempty"`
	}{m, report, ""}
	if err != nil {
		out.Error = err.Error()
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if err != nil {
		return fmt.Errorf("invalid: %w", err)
	}
	return nil
}

func watch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	id := fs.String("id", "", "local job ID (from submit)")
	slurmID := fs.Int("slurm", 0, "Slurm job ID (from submit)")
	backend := fs.String("backend", "auto", "scheduler backend: auto, rest, cli")
	interval := fs.Duration("interval", 15*time.Second, "poll interval (e.g. 10s, 1m)")
	_ = fs.Parse(args)
	if *id == "" || *slurmID < 1 {
		return fmt.Errorf("watch requires -id <local-job-id> and -slurm <slurm-job-id>")
	}
	scheduler, backendName, err := selectScheduler(*backend)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Printf("clutch watch backend=%s id=%s slurm=%d", backendName, *id, *slurmID)
	w := supervisor.Supervisor{Scheduler: scheduler, Store: st, Metrics: &telemetry.Metrics{}, PollInterval: *interval}
	if err := w.Watch(ctx, *id, *slurmID); err != nil {
		return err
	}
	j, err := st.Get(ctx, *id)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(j)
}

func status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	id := fs.String("id", "", "local job ID (from submit)")
	slurmID := fs.Int("slurm", 0, "Slurm job ID (from submit)")
	backend := fs.String("backend", "auto", "scheduler backend: auto, rest, cli")
	refresh := fs.Bool("refresh", false, "query Slurm once and update the local record")
	_ = fs.Parse(args)
	if *id == "" {
		return fmt.Errorf("status requires -id <local-job-id>")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if *refresh {
		if *slurmID < 1 {
			j, err := st.Get(ctx, *id)
			if err != nil {
				return err
			}
			*slurmID = j.SlurmID
		}
		if *slurmID < 1 {
			return fmt.Errorf("no Slurm ID known for job %s (not submitted yet?)", *id)
		}
		scheduler, _, err := selectScheduler(*backend)
		if err != nil {
			return err
		}
		states, err := scheduler.State(ctx, *slurmID)
		if err != nil {
			_ = st.UpdateState(ctx, *id, "UNKNOWN", err.Error(), *slurmID)
			return fmt.Errorf("slurm state: %w", err)
		}
		if len(states) > 0 {
			// Map through the supervisor's terminal rules via a one-shot
			// watch with zero interval? Simpler: store raw Slurm state when
			// non-terminal, canonical local state otherwise.
			w := supervisor.Supervisor{Scheduler: scheduler, Store: st, PollInterval: time.Second}
			_ = w
			local := supervisor.ToLocal(states[0])
			_ = st.UpdateState(ctx, *id, local, "", *slurmID)
		}
	}
	j, err := st.Get(ctx, *id)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(j)
}

func cancel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	id := fs.String("id", "", "local job ID (from submit)")
	slurmID := fs.Int("slurm", 0, "Slurm job ID (from submit)")
	backend := fs.String("backend", "auto", "scheduler backend: auto, rest, cli")
	_ = fs.Parse(args)
	if *id == "" && *slurmID < 1 {
		return fmt.Errorf("cancel requires -id <local-job-id> and/or -slurm <slurm-job-id>")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if *slurmID < 1 {
		j, err := st.Get(ctx, *id)
		if err != nil {
			return err
		}
		*slurmID = j.SlurmID
	}
	if *slurmID < 1 {
		return fmt.Errorf("no Slurm ID known (job not submitted?)")
	}
	scheduler, backendName, err := selectScheduler(*backend)
	if err != nil {
		return err
	}
	log.Printf("clutch cancel backend=%s slurm=%d", backendName, *slurmID)
	if err := scheduler.Cancel(ctx, *slurmID); err != nil {
		return err
	}
	if *id != "" {
		_ = st.UpdateState(ctx, *id, "CANCELLED", "cancelled by operator", *slurmID)
		j, _ := st.Get(ctx, *id)
		return json.NewEncoder(os.Stdout).Encode(j)
	}
	fmt.Printf("cancelled slurm job %d\n", *slurmID)
	return nil
}

func listJobs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	state := fs.String("state", "", "filter by local state (QUEUED, RUNNING, DONE, FAILED, ...)")
	_ = fs.Parse(args)
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	jobs, err := st.ListByState(ctx, strings.ToUpper(strings.TrimSpace(*state)))
	if err != nil {
		return err
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	return json.NewEncoder(os.Stdout).Encode(jobs)
}

// logs resolves a job's log files from the database so no ID ever has to
// be pasted into tail -f by hand:
//
//	clutch logs -manifest <case>/manifest.yaml -latest          # stream newest job
//	clutch logs -manifest <case>/manifest.yaml -id <local-id>   # stream one job
//	tail -f $(clutch logs -manifest <case>/manifest.yaml -latest -follow=false)
func logs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest path (sets default -db to <manifest-dir>/foam-clutch.db)")
	dbPath := fs.String("db", "", "SQLite database path")
	id := fs.String("id", "", "local job ID (from submit/run)")
	latest := fs.Bool("latest", false, "use the most recently submitted job")
	follow := fs.Bool("follow", true, "stream new lines like tail -f (false prints paths only)")
	interval := fs.Duration("interval", 5*time.Second, "stream poll interval")
	_ = fs.Parse(args)

	db := strings.TrimSpace(*dbPath)
	if db == "" {
		if *manifestPath != "" {
			db = filepath.Join(filepath.Dir(*manifestPath), "foam-clutch.db")
		} else {
			db = "foam-clutch.db"
		}
	}
	if *id == "" && !*latest {
		return fmt.Errorf("logs requires -id <local-job-id> or -latest")
	}
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	defer st.Close()
	var j store.Job
	if *latest {
		jobs, err := st.List(ctx)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return fmt.Errorf("no jobs in %s", db)
		}
		j = jobs[0]
	} else {
		j, err = st.Get(ctx, *id)
		if err != nil {
			return err
		}
	}
	paths := logPaths(j)
	if !*follow {
		fmt.Println(strings.Join(paths, " "))
		return nil
	}
	log.Printf("job id=%s slurm=%d state=%s run=%s", j.ID, j.SlurmID, j.State, j.RunPath)
	for _, p := range paths {
		log.Printf("log: %s", p)
	}
	wctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	tailRunLogs(wctx, j.RunPath, j.SlurmID, *interval)
	return nil
}

// logPaths returns every log address for a job: the solver log plus the
// Slurm output and error files Slurm was told to write.
func logPaths(j store.Job) []string {
	return []string{
		filepath.Join(j.RunPath, "log.solver"),
		filepath.Join(j.RunPath, fmt.Sprintf("slurm-%d.out", j.SlurmID)),
		filepath.Join(j.RunPath, fmt.Sprintf("slurm-%d.err", j.SlurmID)),
	}
}

// initManifest scaffolds a generic manifest for any OpenFOAM project. The
// solver is an arbitrary executable name (foamRun, simpleFoam, a custom
// solver); nothing is restricted to a built-in list.
func initManifest(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("o", "manifest.yaml", "output manifest path")
	name := fs.String("name", "myCase", "Slurm/local job name")
	casePath := fs.String("case", "./case", "case directory (relative to the manifest)")
	solver := fs.String("solver", "foamRun", "solver executable (foamRun, simpleFoam, myCustomSolver, ...)")
	partition := fs.String("partition", "", "Slurm partition (empty = cluster default)")
	nodes := fs.Int("nodes", 1, "node count")
	tasks := fs.Int("tasks", 4, "tasks per node")
	minutes := fs.Int("time", 60, "walltime in minutes")
	checkpoint := fs.Int("checkpoint", 600, "USR1 checkpoint seconds before walltime (0 disables)")
	_ = fs.Parse(args)
	doc := fmt.Sprintf(`apiVersion: foam-clutch/v1alpha1
name: %s

case:
  path: %s
  # include: ["Allrun"]  # extra top-level case files to stage

resources:
  partition: %s
  account: ""
  nodes: %d
  tasksPerNode: %d
  timeLimitMinutes: %d
  # sbatchExtra: ["--gres=gpu:1"]

solver:
  name: %s
  arguments: []
  fileHandler: collated   # collated, uncollated, none
  checkpointSeconds: %d

mesh:
  strategy: auto  # auto, blockMesh, custom, none

post:
  reconstruct: false
  # commands: ["foamPostProcess -func sample"]

runtime:
  foamBashrc: %s
  threadsPerRank: 1
  mpi:
    launcher: mpirun  # mpirun or srun
    flavour: pmix     # srun --mpi value; mpirun ignores it

storage:
  cacheDir: ./.foam-cache
  runDir: ./.foam-runs
`, *name, *casePath, *partition, *nodes, *tasks, *minutes, *solver, *checkpoint, manifest.DefaultFoamBashrc)
	if err := os.WriteFile(*out, []byte(doc), 0o640); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	_ = fs.Parse(args)
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	apiToken := strings.TrimSpace(os.Getenv("FOAM_CLUTCH_API_TOKEN"))
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := st.List(r.Context()); err != nil {
			http.Error(w, "db not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		if apiToken != "" && r.Header.Get("Authorization") != "Bearer "+apiToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		jobs, err := st.ListByState(r.Context(), strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("state"))))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if jobs == nil {
			jobs = []store.Job{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jobs)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": Version})
	})
	srv := &http.Server{
		Addr: *addr, Handler: withLogging(mux),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("clutch listening on %s version=%s", *addr, Version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  clutch -manifest manifest.yaml [-db foam-clutch.db] [-backend auto|rest|cli] [-tail]")
	fmt.Fprintln(os.Stderr, "  clutch it -manifest manifest.yaml [-db foam-clutch.db] [-backend auto|rest|cli] [-tail]")
	fmt.Fprintln(os.Stderr, "  clutch run -manifest manifest.yaml [-db foam-clutch.db] [-backend auto|rest|cli] [-tail]")
	fmt.Fprintln(os.Stderr, "  clutch validate -manifest manifest.yaml")
	fmt.Fprintln(os.Stderr, "  clutch submit -manifest manifest.yaml [-db foam-clutch.db] [-backend auto|rest|cli] [-watch]")
	fmt.Fprintln(os.Stderr, "  clutch watch -db foam-clutch.db -id <job-id> -slurm <slurm-id>")
	fmt.Fprintln(os.Stderr, "  clutch status -db foam-clutch.db -id <job-id> [-refresh] [-slurm <id>]")
	fmt.Fprintln(os.Stderr, "  clutch cancel -db foam-clutch.db -id <job-id> [-slurm <id>]")
	fmt.Fprintln(os.Stderr, "  clutch list -db foam-clutch.db [-state DONE]")
	fmt.Fprintln(os.Stderr, "  clutch logs -manifest manifest.yaml [-id <job-id> | -latest] [-follow=false]")
	fmt.Fprintln(os.Stderr, "  clutch init -o manifest.yaml -solver foamRun -case ./case")
	fmt.Fprintln(os.Stderr, "  clutch serve [-addr :8080] [-db foam-clutch.db]")
	fmt.Fprintln(os.Stderr, "  clutch version")
}
