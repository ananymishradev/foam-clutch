# Foam Clutch, explained plainly

## The short version

Foam Clutch is a small operations layer around OpenFOAM and Slurm.

OpenFOAM performs the simulation. Slurm decides when and where the simulation
runs. Foam Clutch sits between them and makes the workflow safer and easier to
repeat:

```text
case + manifest
       │
       ▼
check the case before spending queue time
       │
       ▼
make a stable cached copy
       │
       ▼
make a private working copy for this run
       │
       ▼
record the job locally
       │
       ▼
submit it to Slurm
       │
       ▼
checkpoint, requeue, and watch the result
```

The project is deliberately not trying to replace OpenFOAM, Slurm, ParaView,
Prometheus, or a cluster administrator. It connects those pieces and gives
them a consistent workflow.

## Why this project exists

Large simulations fail in expensive ways:

- a malformed dictionary reaches the queue and consumes allocation time;
- two runs accidentally write into the same directory;
- a wall-time limit kills a solver without a usable checkpoint;
- a repeated parameter sweep rebuilds the same mesh;
- a service restart loses track of a job already submitted to Slurm;
- a user case loads code when the operator expected only data;
- many processor directories overload a parallel filesystem;
- performance problems remain invisible until a long production run.

Foam Clutch addresses the first group of problems now. The repository also
documents the larger performance and operations roadmap for OpenFOAM workloads.

## How the implemented workflow behaves

### 1. The manifest is the contract

The manifest says what should run:

- which case to use;
- which solver to execute;
- how many nodes and tasks to request;
- which Slurm partition and account to use;
- where the cache and run directories belong.

Unknown YAML fields are rejected. Relative paths are resolved relative to the
manifest, not relative to whatever directory happens to launch the command.
This makes automation less surprising.

The current schema is intentionally small:

```yaml
apiVersion: foam-clutch/v1alpha1
name: cavity
case:
  path: ./case
resources:
  nodes: 1
  tasksPerNode: 8
  timeLimitMinutes: 60
solver:
  name: simpleFoam
```

The schema will need versioning as features such as preprocessing, post-
processing, container selection, and output retention become first-class.

### 2. Preflight is a cheap rejection step

Before submission, Foam Clutch checks that the case looks like an OpenFOAM
case. It expects:

```text
0/
constant/
system/
```

It also walks the files and rejects known mechanisms that can load or compile
code:

```text
#codeStream
#calc
codedFixedValue
codedMixed
libs
```

The reason is simple: an OpenFOAM dictionary is not always passive
configuration. Some directives can execute arbitrary code. A multi-user
service must make that behavior explicit and controlled.

Symlinks are inspected as well. A link that points outside the case is rejected
because it can unexpectedly expose or modify unrelated data.

If configured, `foamDictionary` and `checkMesh` can be run during preflight.
The current implementation records tool failures as preflight findings. A
future version should parse their output into structured mesh-quality fields and
policy thresholds.

### 3. The cache saves repeated work

Foam Clutch calculates a SHA-256 digest from:

- each relative file name;
- each file's contents.

That digest becomes the cache key:

```text
.foam-cache/<sha256>/
```

If the same case is submitted again, the cache can be reused. This is useful
for parameter studies and repeated solver attempts.

The cache is not used as the active working directory. Instead, Foam Clutch
materializes a separate run directory:

```text
.foam-runs/<local-job-id>/
```

That separation matters. The checkpoint handler edits `system/controlDict` and
the solver writes logs and time directories. A shared cache must remain a stable
input artifact; it should not become a mutable run directory.

### 4. SQLite records the local truth

The current store uses SQLite with WAL mode. It records:

- a local UUID;
- the human-readable job name;
- the local state;
- the manifest path;
- the case hash;
- the Slurm job ID;
- the last error;
- the last update time.

The service writes a `STAGED` record before it calls Slurm. After a successful
submission it updates the record to `QUEUED` and stores the Slurm ID. If Slurm
rejects the request, the local record becomes `FAILED`.

This is enough for a single service instance or a local development workflow.
It is not yet a complete distributed state machine. For multiple replicas,
the project should move to Postgres, add ownership/leases, and use an outbox
for events that must survive crashes.

### 5. Slurm is accessed through an adapter

The scheduler interface is intentionally small:

```go
Submit
State
Cancel
```

The current implementation uses `slurmrestd` and sends:

- the Slurm user name;
- a JWT token;
- job resource values;
- the working directory;
- the generated solver script.

The API version matters. Slurm REST schemas change between releases. The
default examples use `v0.0.43`, associated with Slurm 25.05, but the target
cluster's `/openapi/v3` document is authoritative.

The client supports both normal HTTP and a Unix-domain socket transport. It
validates configuration at startup, bounds response sizes, respects request
contexts, validates job inputs, and surfaces Slurm API errors even when the
HTTP status itself is successful.

### 6. Checkpointing is cooperative

Slurm can warn the batch process before wall time expires. Foam Clutch's solver
script catches `USR1` and asks OpenFOAM to write immediately:

```bash
foamDictionary -entry stopAt -set writeNow system/controlDict
```

The script marks the run, waits for the solver to exit, and calls:

```bash
scontrol requeue "$SLURM_JOB_ID"
```

The next attempt changes `startFrom` to `latestTime`.

This is not magic checkpointing. It depends on OpenFOAM honoring runtime
dictionary changes, writing a valid time directory, and the cluster allowing
requeue. The solver and control dictionary must be configured appropriately.

### 7. Supervision translates cluster state

The supervisor polls Slurm and maps scheduler states into local states:

| Slurm state | Local state |
|---|---|
| `PENDING` | `QUEUED` |
| `RUNNING` | `RUNNING` |
| `COMPLETED` | `DONE` |
| `FAILED` | `FAILED` |
| `CANCELLED` | `CANCELLED` |
| `TIMEOUT` | `REQUEUED` |

Unknown states are preserved rather than silently treated as success. A
production supervisor should also reconcile jobs after restart, handle
accounting records for jobs that have left `slurmctld`, and distinguish a
checkpoint requeue from a failed requeue.

### 8. Telemetry is intentionally small today

The metrics endpoint emits Prometheus text counters for:

- submissions;
- preflight failures;
- cache hits;
- cache misses;
- requeues.

The architecture described in `ARCHITECTURE.md` goes further: parse solver logs,
publish residuals and time-per-step, detect stalls and NaNs, and connect
Prometheus to Grafana. Those are the next operational layers, not silently
claimed features of the current implementation.

## What belongs in production

A production deployment should add the following around the current core:

### Identity and access

- API authentication;
- per-user or per-project authorization;
- Slurm account and partition policy;
- filesystem isolation;
- audit records without secrets.

### Secrets

- refreshable JWTs;
- secret storage outside command-line arguments;
- no tokens in logs;
- short token lifetimes;
- rotation procedures.

### Reliability

- Postgres for multiple service replicas;
- leases for job ownership;
- durable outbox events;
- restart reconciliation;
- retry policies with bounded backoff;
- cleanup of abandoned run directories;
- cache retention and quota policies.

### OpenFOAM policy

- solver-specific dictionary schemas;
- complete mesh-quality thresholds;
- explicit allow-lists for any code-capable directive;
- immutable, verified runtime images;
- output and result retention rules.

### Operations

- structured logs;
- alerts for stalled jobs, failed preflight, checkpoint loops, and disk usage;
- Grafana dashboards;
- node health checks;
- staging-cluster integration tests;
- strong and weak scaling benchmarks.

## Performance direction

The design notes in `ARCHITECTURE.md` target HPC behavior that is easy to overlook:

- use `scotch` or topology-aware decomposition;
- avoid running active cases from a home directory;
- prefer collated I/O for large parallel jobs;
- use node-local storage where possible;
- avoid serial reconstruction for large cases;
- pin MPI ranks to physical cores;
- benchmark partial node occupancy;
- measure MPI wait time rather than assuming more ranks are faster;
- choose rank counts from cells per rank and observed scaling;
- cache meshes and decompositions when the inputs are unchanged.

For a real autotuner, the system should run controlled probe cases, record
machine and solver metadata, measure time per simulated second, and select a
configuration from observed strong-scaling efficiency rather than a universal
rule.

## How to read the repository

Start with `README.md` when you want to run the project.

Read `internal/manifest` to understand the input contract.

Read `internal/preflight` to understand what is rejected before queue time is
spent.

Read `internal/stage` to understand why cache paths and run paths are separate.

Read `internal/store` to understand what survives a process restart.

Read `internal/service` to follow the end-to-end workflow.

Read `client.go` to understand the Slurm REST boundary.

Read `internal/supervisor` to understand how remote state becomes local state.

Read `ARCHITECTURE.md` for the broader system architecture and HPC performance plan.

## Honest status

The repository is a useful, tested control-plane foundation. It is not yet a
finished multi-tenant platform, and it should not be exposed as an
unauthenticated submission API. The local workflow is real; the production
hardening items above are the work required to operate it responsibly on a
shared cluster.
