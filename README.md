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

- Go 1.26 or newer.
- OpenFOAM available on the execution host or inside the configured runtime.
- Slurm with `slurmrestd` enabled.
- A valid Slurm REST API version, such as `v0.0.43`.
- A Slurm JWT for submission.
- `srun`, `scontrol`, and the selected OpenFOAM tools available to jobs.

The repository also contains `openfoam.def` for building an Apptainer image.
Container/MPI compatibility must be verified against the target cluster.

## Project layout

```text
cmd/foam-clutch/
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

Create a file such as `manifest.yaml`:

```yaml
apiVersion: foam-clutch/v1alpha1
name: cavity

case:
  path: ./case

resources:
  partition: compute
  account: research
  nodes: 1
  tasksPerNode: 8
  timeLimitMinutes: 60

solver:
  name: simpleFoam
  arguments: []
  checkpointSeconds: 600

storage:
  cacheDir: ./.foam-cache
  runDir: ./.foam-runs
```

Relative paths are resolved relative to the manifest file. The case directory
must contain:

```text
case/
├── 0/
├── constant/
└── system/
```

Important fields:

| Field | Meaning |
|---|---|
| `apiVersion` | Must be `foam-clutch/v1alpha1`. |
| `name` | Slurm job name and local job name. |
| `case.path` | OpenFOAM case directory. |
| `resources.partition` | Slurm partition. |
| `resources.account` | Slurm accounting account. |
| `resources.nodes` | Number of nodes. |
| `resources.tasksPerNode` | MPI tasks per node. |
| `resources.timeLimitMinutes` | Slurm time limit. |
| `solver.name` | Solver executable, for example `simpleFoam`. |
| `solver.arguments` | Additional solver arguments. |
| `storage.cacheDir` | Content-addressed cache root. |
| `storage.runDir` | Per-job working directory root. |

## Local validation

Format and test the project:

```bash
go test ./...
go vet ./...
go test -race ./...
```

Validate a case before submitting:

```bash
go run ./cmd/foam-clutch validate -manifest manifest.yaml
```

The command prints JSON containing the parsed manifest, preflight report, and
an error when validation fails.

## Submit a job

Set the Slurm connection values:

```bash
export SLURM_REST_URL=http://slurmrestd.example:6820
export SLURM_API_VERSION=v0.0.43
export SLURM_USER="$USER"
export SLURM_JWT="$SLURM_JWT"
```

Submit:

```bash
go run ./cmd/foam-clutch submit \
  -manifest manifest.yaml \
  -db foam-clutch.db
```

Submit and keep polling Slurm:

```bash
go run ./cmd/foam-clutch submit \
  -manifest manifest.yaml \
  -db foam-clutch.db \
  -watch
```

The command returns a local job ID, case hash, run path, cache path, and Slurm
job ID.

The current CLI uses a static `SLURM_JWT` value for simplicity. A production
deployment should provide a refreshable token callback and should not place
long-lived credentials in shell history, process listings, or shared logs.

## Run the service

Start the local HTTP service:

```bash
go run ./cmd/foam-clutch serve \
  -addr :8080 \
  -db foam-clutch.db
```

Available endpoints:

```text
GET /healthz   liveness response
GET /metrics   Prometheus text exposition
GET /jobs      persisted jobs as JSON
```

Example:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/metrics
curl http://localhost:8080/jobs
```

The HTTP service currently exposes health, metrics, and read-only job listing.
It does not expose anonymous job submission. Add authentication and
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

The generated solver script:

1. Starts the solver under `srun`.
2. Receives `USR1` before the Slurm time limit.
3. Sets `stopAt` to `writeNow`.
4. Marks the run as checkpoint-requested.
5. Waits for the solver to exit cleanly.
6. Calls `scontrol requeue` for the current Slurm job.
7. Starts from `latestTime` on the next execution.

This requires:

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
