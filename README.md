# Foam Clutch

Foam Clutch is a small Go control plane for running OpenFOAM cases on a Slurm
cluster. It validates a case before submitting it, stores a reproducible copy
in a content-addressed cache, creates an isolated run directory, persists job
state in SQLite, submits through `slurmrestd`, exposes Prometheus metrics, and
can watch the Slurm job until it finishes.

The project is intentionally practical: it provides a dependable foundation for
HPC simulation workflows without hiding cluster-specific behavior behind a
large framework.

## What it does

The current workflow is:

```text
manifest.yaml
    │
    ▼
strict manifest validation
    │
    ▼
case preflight
    │
    ▼
SHA-256 cache and isolated run directory
    │
    ▼
SQLite job record
    │
    ▼
slurmrestd submission
    │
    ▼
OpenFOAM solver with checkpoint/requeue handling
    │
    ▼
Slurm state supervision and Prometheus metrics
```

## Current capabilities

- Strict YAML manifests using `foam-clutch/v1alpha1`.
- Required OpenFOAM case layout checks for `system`, `constant`, and `0`.
- Rejection of unsafe symlinks that escape the case directory.
- Rejection of runtime code-loading directives such as:
  `#codeStream`, `#calc`, `codedFixedValue`, `codedMixed`, and `libs`.
- Optional execution of `foamDictionary` and `checkMesh`.
- SHA-256 content-addressed case caching.
- Cache reuse for identical case contents.
- Isolated run directories so checkpoint edits do not mutate the cache.
- SQLite persistence with WAL mode and a simple job state model.
- Slurm REST submission, state lookup, and cancellation.
- JWT token callback support in the scheduler client.
- `USR1` checkpoint handling and `scontrol requeue`.
- Prometheus-compatible `/metrics`.
- Basic HTTP health and read-only job listing endpoints.
- Unit and integration-style tests for the local workflow.

## Requirements

- Go 1.26 or newer, installed system-wide so `go` is on the default `PATH`:
  ```bash
  sudo rm -rf /usr/local/go
  sudo tar -xzf go1.26.0.linux-amd64.tar.gz -C /usr/local
  sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
  go version  # go version go1.26.0 linux/amd64
  ```
- OpenFOAM available on the execution host or inside the configured runtime
  (this cluster: `/opt/openfoam12/etc/bashrc`, identical on every node).
- Slurm with `sbatch`/`squeue`/`scontrol` on `PATH` (CLI backend, works
  without further setup), and/or `slurmrestd` with a valid REST API version
  (such as `v0.0.43`) plus a Slurm JWT for the REST backend.
- `mpirun` (OpenMPI) and the selected OpenFOAM tools available to jobs.

The repository also contains `openfoam.def` for building an Apptainer image.
Container/MPI compatibility must be verified against the target cluster.

## Install (`clutch` command)

The CLI lives in `cmd/clutch` and installs as `clutch`:

```bash
go install ./cmd/clutch
clutch version
```

Ensure `$(go env GOPATH)/bin` is on `PATH`. This shell has
`~/.local/bin` on `PATH`, so this also works:

```bash
cp "$(go env GOPATH)/bin/clutch" ~/.local/bin/clutch
clutch version
```

Without installing, the equivalent developer form is:

```bash
go run ./cmd/clutch version
```

All examples below use the installed `clutch` command.

## Run it

```bash
clutch it -manifest manifest.yaml
```

`it` is a short alias for `run`. These three forms are identical:

```bash
clutch -manifest manifest.yaml
clutch it -manifest manifest.yaml
clutch run -manifest manifest.yaml
```

## Project layout

```text
cmd/clutch/
  main.go
  CLI commands and the local HTTP service

internal/manifest/
  Manifest types, strict YAML loading, validation, example manifest

internal/preflight/
  OpenFOAM case layout, symlink, directive, and tool checks

internal/stage/
  Content hashing, cache population, and isolated run materialization

internal/store/
  SQLite database schema and job persistence

internal/service/
  End-to-end validation, staging, persistence, and scheduler submission

internal/supervisor/
  Slurm state polling and local state reconciliation

internal/telemetry/
  Prometheus text exposition

client.go
  slurmrestd client and Scheduler interface

solver.sbatch.tmpl
  Standalone Slurm batch template

openfoam.def
  Apptainer definition
```

## Manifest

No solver, mesh tool, MPI launcher, or file handler is hard-coded. The same
schema runs heavySimple (`foamRun` + `incompressibleFluid`), heavyBuoyant
(`foamRun` + `fluid`), any stock solver (`simpleFoam`, `pimpleFoam`,
`buoyantPimpleFoam`, ...), and any custom-compiled solver: `solver.name` is
an arbitrary bare executable, and the batch script is rendered entirely from
manifest fields. Scaffold one with:

```bash
clutch init -o manifest.yaml -solver foamRun -case ./case
```

Full schema (`examples/generic.yaml`):

```yaml
apiVersion: foam-clutch/v1alpha1
name: cavity

case:
  path: ./case
  include: []            # extra top-level inputs to stage (e.g. ["Allrun"])

resources:
  partition: ""         # empty = cluster default
  account: ""
  nodes: 1
  tasksPerNode: 8
  timeLimitMinutes: 60
  sbatchExtra: []       # e.g. ["--gres=gpu:1"], appended to SBATCH + REST Extra

solver:
  name: foamRun         # any executable: foamRun, simpleFoam, myCustomSolver, ...
  arguments: []
  fileHandler: collated # collated, uncollated, none
  checkpointSeconds: 600 # USR1 writeNow N s before walltime; 0 disables
  # container: /images/openfoam.sif

mesh:
  strategy: auto        # auto, blockMesh, custom, none
  # commands: ["snappyHexMesh -overwrite"]

post:
  reconstruct: false
  # commands: ["foamPostProcess -func sample"]

runtime:
  foamBashrc: /opt/openfoam12/etc/bashrc  # or $FOAM_BASHRC; auto-detected
  threadsPerRank: 1
  mpi:
    launcher: mpirun    # mpirun or srun
    flavour: ""         # srun --mpi value; mpirun ignores it

storage:
  cacheDir: ./.foam-cache
  runDir: ./.foam-runs
```

Relative paths are resolved relative to the manifest file. The case directory
must contain:

```text
case/
├── 0/            # or 0.orig (either satisfies preflight)
├── constant/
└── system/
```

Important fields:

| Field | Meaning |
|---|---|
| `apiVersion` | Must be `foam-clutch/v1alpha1`. |
| `name` | Slurm job name and local job name. |
| `case.path` | OpenFOAM case directory. |
| `case.include` | Extra top-level case files/dirs to stage and hash. |
| `resources.partition` | Slurm partition (empty = default). |
| `resources.account` | Slurm accounting account. |
| `resources.nodes` | Number of nodes. |
| `resources.tasksPerNode` | MPI tasks per node. |
| `resources.timeLimitMinutes` | Slurm time limit. |
| `resources.sbatchExtra` | Raw `--key[=value]` SBATCH lines (never repeats managed opts). |
| `solver.name` | Any solver executable, for example `foamRun` or `myCustomSolver`. |
| `solver.arguments` | Additional solver arguments. |
| `solver.fileHandler` | `collated`, `uncollated`, or `none`. |
| `solver.checkpointSeconds` | USR1 lead time; `0` disables checkpoint/requeue. |
| `mesh.strategy` | `auto`, `blockMesh`, `custom`, or `none`. |
| `post.reconstruct` | Run `reconstructPar` after a clean parallel solve. |
| `runtime.foamBashrc` | OpenFOAM env; manifest > `$FOAM_BASHRC` > auto-detect. |
| `runtime.mpi.launcher` | `mpirun` or `srun`. |
| `storage.cacheDir` | Content-addressed cache root. |
| `storage.runDir` | Per-job working directory root. |

Validation also checks the case against the manifest generically: `controlDict`
`application` must equal the manifest solver (or `foamRun` with any `solver
<model>;` physics entry), `decomposeParDict` must match `nodes × tasksPerNode`
for parallel runs, `checkpointSeconds` requires `runTimeModifiable true`, and
`mesh.strategy` must have a mesh source.

## Local validation

Format and test the project:

```bash
go test ./...
go vet ./...
go test -race ./...
```

Validate a case before submitting:

```bash
clutch validate -manifest manifest.yaml
```

The command prints JSON containing the parsed manifest, preflight report, and
an error when validation fails.

## Run an OpenFOAM project with a single command

```bash
clutch run -manifest /home/shared/openfoam/heavySimple/manifest.yaml
clutch run -manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml
```

That one command validates the manifest and case, stages it into the
content-addressed cache, materializes an isolated run directory, submits to
Slurm, and watches until DONE/FAILED/CANCELLED — printing the local ID, Slurm
ID, run directory, and both log paths. Defaults: `-db` is
`<manifest-dir>/foam-clutch.db`, `-backend auto`. Add `-tail` to stream
`log.solver` and `slurm-<id>.out` while watching:

```bash
clutch run -manifest /home/shared/openfoam/heavySimple/manifest.yaml -tail
clutch run -manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml -tail
```

`submit`/`watch`/`status`/`cancel` below are the same pipeline split into
steps for scripting; `run` is just `validate + submit -watch` in one call.

## Submit a job (step-by-step)

Pick a backend first (see Scheduler backends below). For the REST backend,
set the Slurm connection values (`SLURM_JWT_FILE` is preferred over
`SLURM_JWT` because it is re-read on every request, so rotated tokens are
picked up without a restart; `SLURM_SOCKET` selects Unix-socket transport):

```bash
export SLURM_REST_URL=http://slurmrestd.example:6820
export SLURM_API_VERSION=v0.0.43
export SLURM_USER="$USER"
export SLURM_JWT_FILE="$HOME/.slurm/token"  # or SLURM_JWT for short-lived setups
```

Submit (CLI backend shown; needs nothing but Slurm on `PATH`):

```bash
clutch submit \
  -manifest manifest.yaml \
  -db foam-clutch.db \
  -backend cli
```

Submit and keep polling Slurm until the job reaches a terminal state:

```bash
clutch submit \
  -manifest manifest.yaml \
  -db foam-clutch.db \
  -backend cli \
  -watch
```

The command returns a local job ID, case hash, run path, cache path, and Slurm
job ID.

Reconcile the state of an already-submitted job (updates the DB record to
DONE/FAILED/CANCELLED once Slurm reaches a terminal state):

```bash
clutch watch \
  -db foam-clutch.db \
  -id <local-job-id> \
  -slurm <slurm-job-id>
```

One-shot refresh, cancel, and listing:

```bash
clutch status -db foam-clutch.db -id <local-job-id> -refresh
clutch cancel -db foam-clutch.db -id <local-job-id>
clutch list -db foam-clutch.db -state RUNNING
```

## Scheduler backends

`submit`, `watch`, `status`, and `cancel` accept `-backend auto|rest|cli`
(default `auto`).

- `rest` talks to `slurmrestd` and needs `SLURM_API_VERSION`, `SLURM_USER`,
  `SLURM_JWT` or `SLURM_JWT_FILE`, and `SLURM_REST_URL` or `SLURM_SOCKET`.
- `cli` shells out to `sbatch`/`squeue`/`scontrol`/`sacct`/`scancel` and needs no
  extra configuration. `sacct` is the last-resort state source for finished
  jobs once `slurmctld` forgets them (unavailable when accounting storage is
  disabled, in which case the job is recorded `UNKNOWN` with a bounded retry
  budget).
- `auto` prefers `rest` when the REST variables are all set and falls
  back to `cli` otherwise. Clusters without `slurmrestd` (the common case)
  work out of the box via `cli`.

## Examples: heavySimple and heavyBuoyant on this cluster

Both cases run through the identical generic path — the only differences are
the case directory and the physics model declared in `controlDict`
(`incompressibleFluid` vs `fluid`); the runner executable is `foamRun` in
both manifests and is never special-cased:

```bash
clutch validate -manifest /home/shared/openfoam/heavySimple/manifest.yaml
clutch validate -manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml
clutch run -manifest /home/shared/openfoam/heavySimple/manifest.yaml
clutch run -manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml
squeue
tail -f /home/shared/openfoam/heavySimple/.foam-runs/<job-id>/slurm-<slurm-id>.out
```

The generated job script is a full pipeline driven by the manifest: mesh
(`auto` uses `constant/polyMesh` when present, else `blockMesh` when
`system/blockMeshDict` exists; `custom` runs `mesh.commands`; `none` skips)
→ `decomposePar` for parallel runs (rebuilt when the processor count
mismatches `nodes × tasksPerNode`; skipped for serial `ranks=1`) → solver
`<solver.name> <args> [-parallel] [-fileHandler ...]` via `mpirun` or `srun`
with a `USR1` `writeNow` checkpoint trap and `scontrol requeue` when
`checkpointSeconds > 0` → optional `reconstructPar` and `post.commands`.
The manifest's `decomposeParDict` subdomain count is validated against the
requested ranks before submission, so a mismatch fails fast instead of wasting
queue time. Only `0/` (or `0.orig/`), `constant/`, `system/`, plus explicit
`case.include` tops are staged and hashed; docs, scripts, logs, and `.foam-*`
state never affect the case hash.

## Where Slurm output and error files go

Every clutch run writes three logs inside its isolated run directory.
Slurm expands `%j` to the Slurm job ID, so the addresses are:

```text
<case>/.foam-runs/<local-job-id>/log.solver      # solver residuals (all ranks)
<case>/.foam-runs/<local-job-id>/slurm-<slurm-id>.out  # Slurm stdout: mesh, decompose, batch echo
<case>/.foam-runs/<local-job-id>/slurm-<slurm-id>.err  # Slurm stderr
```

Concrete addresses:

```text
heavySimple:  /home/shared/openfoam/heavySimple/.foam-runs/<local-job-id>/slurm-<slurm-id>.out
              /home/shared/openfoam/heavySimple/.foam-runs/<local-job-id>/slurm-<slurm-id>.err
              /home/shared/openfoam/heavySimple/.foam-runs/<local-job-id>/log.solver
heavyBuoyant: /home/shared/openfoam/heavyBuoyant/.foam-runs/<local-job-id>/slurm-<slurm-id>.out
              /home/shared/openfoam/heavyBuoyant/.foam-runs/<local-job-id>/slurm-<slurm-id>.err
              /home/shared/openfoam/heavyBuoyant/.foam-runs/<local-job-id>/log.solver
```

The same run directory also holds `log.blockMesh`, `log.decomposePar`,
`processor*`, and `foam-clutch.sbatch` (the exact script Slurm executed —
resubmit it with `sbatch` to reproduce).

You never need to paste IDs by hand. The `run` command prints all three
paths at submit time, `-tail` streams them live, and `logs` resolves any
past job from the database:

```bash
# stream the newest heavySimple job (no IDs typed):
clutch logs -manifest /home/shared/openfoam/heavySimple/manifest.yaml -latest

# stream the newest heavyBuoyant job:
clutch logs -manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml -latest

# stream one specific job:
clutch logs -manifest /home/shared/openfoam/heavySimple/manifest.yaml -id <local-job-id>

# plain tail -f with auto-resolved paths (no IDs typed):
tail -f $(clutch logs -manifest /home/shared/openfoam/heavySimple/manifest.yaml -latest -follow=false)
tail -f $(clutch logs -manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml -latest -follow=false)
```

Other layouts for reference:

- **Direct `sbatch run_openfoam.slurm`:** `/home/shared/openfoam/heavySimple/slurm-<JOBID>.out`
  and `slurm-<JOBID>.err` (absolute paths baked into that script).
- **Standalone `solver.sbatch.tmpl`:** `<caseDir>/slurm-%j.out` / `.err`,
  where `<caseDir>` is the template's `CaseDir` field.

Solver residuals live in `<runDir>/log.solver` regardless of backend, so
`tail -f` that file plus the `slurm-*.out` above for mesh/decompose progress.

JWTs are never logged. Prefer `SLURM_JWT_FILE` over `SLURM_JWT` so the token
is re-read on every request, and do not place long-lived credentials in shell
history, process listings, or shared logs.

## Run the service

Start the local HTTP service:

```bash
clutch serve \
  -addr :8080 \
  -db foam-clutch.db
```

Available endpoints:

```text
GET /healthz   liveness response
GET /readyz    readiness (checks the database)
GET /metrics   Prometheus text exposition
GET /jobs      persisted jobs as JSON (?state=RUNNING filters)
GET /version   build version
```

Example:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/metrics
curl http://localhost:8080/jobs
```

Set `FOAM_CLUTCH_API_TOKEN` to require `Authorization: Bearer <token>` on
`/jobs`. The HTTP service exposes health, metrics, and read-only job listing
only. It does not expose anonymous job submission. Add authentication and
authorization before putting it behind a shared network endpoint.

## Cache and run directories

The cache is keyed by a SHA-256 hash of relative file names and file contents.
An unchanged case reuses the same cache directory:

```text
.foam-cache/
└── <sha256>/
```

Every submission receives its own run directory:

```text
.foam-runs/
└── <local-job-id>/
```

The run directory is a copy of the cached case. This is important because the
checkpoint handler edits `system/controlDict`; changing the cache would make
future jobs non-reproducible.

## Checkpoint and requeue behavior

When `solver.checkpointSeconds > 0`, the generated solver script:

1. Starts the solver under `mpirun` or `srun` (per `runtime.mpi`).
2. Receives `USR1` that many seconds before the Slurm time limit.
3. Sets `stopAt` to `writeNow`.
4. Marks the run as checkpoint-requested.
5. Waits for the solver to exit cleanly.
6. Calls `scontrol requeue` for the current Slurm job.
7. Starts from `latestTime` on the next execution.

When `checkpointSeconds: 0`, the `--signal`/`--requeue` directives, the trap,
and the requeue call are all omitted and the solver runs straight through.

This requires (only when checkpointing is enabled):

- `system/controlDict` to be runtime-modifiable;
- a valid `foamDictionary` command;
- Slurm requeue permission;
- a solver that writes a usable latest time before exiting.

The standalone `solver.sbatch.tmpl` remains available for users who want to
render or submit a batch script outside the Go service.

## Security model

OpenFOAM dictionaries can load and compile code. A user-supplied case can
therefore become arbitrary code execution if it is run without restrictions.
The default preflight rejects known code-loading directives and symlinks that
escape the case directory.

This is only a first security boundary. A production multi-user deployment
should also:

- authenticate every API caller;
- authorize Slurm accounts, partitions, and filesystem paths;
- run jobs with the submitting user's identity or an explicitly audited
  service identity;
- isolate case data and temporary files by user;
- use a read-only or verified container image;
- apply CPU, memory, disk, and wall-clock limits;
- avoid exposing the SQLite database or cache over shared writable paths;
- log submissions and state transitions without logging JWTs;
- scan archives for path traversal before extraction;
- review any additional OpenFOAM directives introduced by the target version.

## Limitations

This is a working control-plane foundation, not a complete hosted SaaS product.
The following areas still need deployment-specific work:

- token refresh and secret management;
- HTTP authentication and authorization;
- real API handlers for authenticated submit/cancel operations;
- Postgres or another multi-instance database if SQLite is insufficient;
- transactional outbox/event delivery;
- scheduler reconciliation after service restarts;
- full `foamDictionary` schema validation per solver;
- complete `checkMesh` result parsing and quality thresholds;
- log parsing for residuals, Courant number, and step time;
- Grafana dashboards and alert rules;
- stage-out/result indexing;
- autotuning and scaling benchmarks;
- multi-node container/MPI validation;
- cleanup and retention policies.

## Development notes

The scheduler boundary is the `Scheduler` interface:

```go
type Scheduler interface {
    Submit(context.Context, JobSpec) (int, error)
    State(context.Context, int) ([]string, error)
    Cancel(context.Context, int) error
}
```

This keeps the orchestration layer independent of Slurm REST details and leaves
room for an `sbatch`/`squeue`/`sacct` adapter or another scheduler later.

## License

No license file is currently included. Add an explicit license before
redistributing the project.
