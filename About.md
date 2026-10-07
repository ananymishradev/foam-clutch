Here's the maximal version. I'm assuming Slurm, a recent OpenFOAM build, an MPI-capable fabric (InfiniBand or similar), and a parallel filesystem (Lustre or GPFS). If any of those differ, only the backend adapters change.

## Architecture

```
AM PravaH UI/API
      │  (case dir + manifest.yaml)
      ▼
[1 Ingest & Preflight] → [2 Planner/Autotuner] → [3 Stage-in + Cache]
                                                      │
                                    [4 Scheduler Adapter (Slurm/PBS)]
                                                      │
        preprocess job ──► solver job (checkpoint/requeue loop) ──► post job
                                                      │
              [5 Telemetry → Prometheus/Grafana]  [6 Failure Supervisor]
                                                      ▼
                                          [7 Stage-out + Result Index]
```

## 1. Ingest and preflight
Reject bad cases before they cost queue time.
- Content-hash the case (geometry, `system/`, `constant/`, `0/`) for caching and reproducibility.
- Validate dictionaries with `foamDictionary` and a schema per solver. Check that every field in `0/` has boundary conditions for every patch.
- Run `checkMesh` on a login or small-allocation job, never on compute nodes at scale. Fail fast on non-orthogonality, negative volumes, or missing patches.
- Estimate memory as cells × ~1–1.5 KB for a typical incompressible/VOF case, then size nodes against it.

## 2. Planner and autotuner
This is where most of the performance comes from.
- **Ranks:** OpenFOAM stops scaling efficiently below roughly 20–50k cells per rank, because communication in the pressure solve dominates. Pick the rank count from cell count first, then refine empirically.
- **Probe runs:** Run about 20 timesteps at 3–4 node counts, fit a strong-scaling curve, and choose the point that meets your efficiency target (say ≥70%). Cache the result keyed by (solver, mesh size class, machine).
- **Partially filled nodes:** OpenFOAM is memory-bandwidth bound, so using 75% of the cores per node often gives better throughput per core. Include `ppn` in the probe sweep.
- **Decomposition:** Use `scotch` as the default, with `hierarchical` as an option that matches the node/socket topology. For additive manufacturing, the active region (melt pool, moving heat source) moves, so static decomposition drifts out of balance. If AM PravaH uses dynamic refinement, schedule periodic `redistributePar` or use `dynamicLoadBalance` where your OpenFOAM version supports it.

## 3. Stage-in and cache
- A content-addressed mesh cache lets parameter sweeps skip `blockMesh`/`snappyHexMesh`/`decomposePar` entirely when only boundary conditions or physics changed. For sweeps this is often the single biggest saving.
- Run active jobs from scratch or node-local NVMe, never from home. Stage out only what's needed.
- Set Lustre striping on the run directory (`lfs setstripe -c <n>`) so collated output isn't bottlenecked on one OST.

## 4. Job template
One job per phase, chained with `--dependency=afterok`. Preprocessing is typically serial or low-rank and shouldn't hold a big allocation.

```bash
#!/bin/bash
#SBATCH --nodes={{nodes}} --ntasks-per-node={{ppn}} --exclusive
#SBATCH --hint=nomultithread --requeue
#SBATCH --signal=B:USR1@600          # warn 10 min before walltime
set -euo pipefail
source {{foam_bashrc}}
export OMP_NUM_THREADS=1
export FOAM_IORANKS='{{ioranks}}'    # one I/O rank per node, e.g. (0 32 64 96)

# On warning: ask OpenFOAM to write and stop cleanly
trap 'foamDictionary -entry stopAt -set writeNow system/controlDict' USR1

srun --mpi=pmix --cpu-bind=cores {{solver}} -parallel \
     -fileHandler collated > log.solver 2>&1 &
PID=$!
wait $PID || true     # interrupted by trap
wait $PID             # actual exit
```

The Supervisor sees a clean `writeNow` exit before walltime and resubmits with `startFrom latestTime`. That gives you effectively unlimited-length runs across queue limits.

## 5. I/O settings that matter
- `-fileHandler collated` with `FOAM_IORANKS` avoids the classic problem of `processor*/` directories exhausting inodes and metadata servers.
- `writeFormat binary; writeCompression off;` and a sensible `writeInterval`. Don't write every step.
- Use function objects (probes, patch integrals, `surfaces`) for in-situ output so you rarely need full-field dumps.
- Avoid serial `reconstructPar` on large cases. Convert directly with `foamToVTK -parallel` or open the decomposed case in ParaView.

## 6. Solver and MPI tuning
Expose these as profiles the planner picks from.
- GAMG for pressure, with `nCellsInCoarsestLevel`, `agglomerator faceAreaPair`, and `processorAgglomerator` (`masterCoarsest` or `procFaces`) tuned. Coarse-level collectives are a common scaling wall.
- `commsType nonBlocking`, and test `floatTransfer`.
- Pin ranks (`--cpu-bind=cores`, map by socket), one rank per physical core, and match UCX/OFI settings to the fabric.
- Build the image per CPU microarchitecture with `-O3 -march=...`. An Apptainer image per target (for example Zen vs Intel) with host MPI bind-mounted gives reproducibility without losing performance.

## 7. Telemetry
- A log tailer parses `log.solver` into metrics: time per step, Courant number, residuals per field, and iteration counts. Push these to Prometheus and show them in Grafana and in the AM PravaH UI.
- **Anomaly rules:** time per step rising more than ~25% over its baseline suggests load imbalance, so trigger a rebalance. A Courant spike or NaN means kill early and report. A stalled log means a node hang, so requeue with `--exclude=<bad node>`.
- Add a node-health prolog (memory, fabric link, filesystem mount) so you stop landing on bad nodes.

## 8. Benchmarking harness
This is your project's deliverable as much as the runner is.
- Automate strong-scaling and weak-scaling sweeps and emit speedup, parallel efficiency, and cost per simulated second.
- Profile with OpenFOAM's built-in `-profiling`, plus mpiP or Score-P, to see the MPI-wait fraction. If MPI time exceeds ~30–40%, you've passed the scaling limit for that case.

## 9. Control plane
Keep it boring: a small service (Go fits well, given your distributed-systems track; Python/FastAPI also works) with a job state machine (`PREFLIGHT → PLANNED → STAGED → PREPROC → RUNNING ⇄ REQUEUED → POST → DONE/FAILED`) stored in Postgres or SQLite. Talk to the cluster through `slurmrestd` or `sbatch`/`sacct`, behind a scheduler interface so PBS support is just another adapter.

## Suggested build order
Don't build all of this first. Over-engineering works best when each layer earns its place:
1. Manifest → `sbatch` → run → stage-out (works end to end).
2. Chained preprocess/solve/post, collated I/O, pinning.
3. Checkpoint/requeue loop and log parser.
4. Probe-based autotuner and scaling benchmarks, which give you real numbers to show.
5. Mesh cache, telemetry dashboard, dynamic rebalancing.

Read the diagram top to bottom. The dashed line from the solver job up to the supervisor is the checkpoint, requeue, and failure loop. Every box is clickable if you want a deeper dive.

## Tech stack

| Layer | Choice | Why |
|---|---|---|
| Control plane language | Go | One static binary you can drop on a login or edge node without root. It handles concurrent job polling well, and it overlaps with your distributed-systems learning. |
| API | REST (chi) with an OpenAPI spec, plus SSE for live progress | AM PravaH can stream residuals and step times without polling. |
| Job state | PostgreSQL (SQLite for the MVP), migrations with goose | The state machine is the core. Use an outbox table so a crash between "submitted to Slurm" and "recorded" can't lose a job. |
| Internal events | Postgres `LISTEN/NOTIFY` | NATS or Kafka would be overkill for this scale. |
| Slurm interface | `slurmrestd` (JWT) when enabled, with an `sbatch`/`squeue`/`sacct --json` fallback behind one Go interface | Many clusters don't expose `slurmrestd`, so the fallback matters. |
| Slurm features | `--dependency=afterok`, `--signal=B:USR1@600`, `--requeue`, `--exclusive`, `--hint=nomultithread`, `--mpi=pmix`, job arrays for sweeps | These give you the preprocess→solve→post chain, clean pre-walltime checkpoints, and cheap parameter studies. |
| Runtime | OpenFOAM in an Apptainer image (one per CPU microarchitecture), host MPI bind-mounted, UCX or OFI matched to the fabric | You get reproducibility without losing native-fabric performance. |
| Decomposition | Scotch, with hierarchical as the topology-aware option | `redistributePar` handles rebalancing as the heat source moves. |
| Storage | Lustre or GPFS scratch with striping, node-local NVMe where available, a content-addressed mesh cache (SHA-256 or BLAKE3, LRU eviction) | The cache makes sweeps that reuse a mesh skip meshing entirely. |
| Results | Filesystem plus a metadata index in Postgres; optional MinIO for archival | Keeps big fields on the parallel FS and only KPIs and paths in the database. |
| Telemetry | Go log tailer → Prometheus (textfile or `remote_write`, since clusters often block inbound scrapes) → Grafana, with `sacct` and `seff` for efficiency | Push-style metrics work behind typical HPC firewalls. |
| Profiling | OpenFOAM `-profiling`, mpiP, Score-P with Scalasca, `perf` | These show the MPI-wait fraction and tell you where scaling breaks. |
| Analysis | Python (pandas, matplotlib), results stored as Parquet | Scaling and efficiency reports for the benchmarking deliverable. |
| Dev and test | `slurm-docker-cluster` locally, plus tiny regression cases (cavity, dam break) with golden residual checks | You can build and test the whole system on your laptop before touching the real cluster. |

## Three things to settle early

1. **User-supplied OpenFOAM dictionaries can execute code.** Features like `#codeStream`, `#calc`, `coded*` boundary conditions, and `libs (...)` entries load and run arbitrary code at runtime. Since AM PravaH users author these files, preflight should scan for and reject (or allow-list) them, and sanitize tarballs for symlinks and path escapes. This is a security requirement, not a nicety.
2. **Where the daemon runs.** Many HPC centers forbid long-running processes on login nodes. Check the policy. If it applies, run the control plane on a separate VM or edge node and talk to Slurm over `slurmrestd` or SSH.
3. **Which user the jobs run as.** Submitting as a shared service account is simpler, but per-user accounting usually needs jobs submitted under the real user's Slurm account. This is a conversation with the cluster admins.

## Cluster integration notes

The repository's current implementation is the `slurmrestd` client in
`client.go`, not the full control plane described above. The remaining text in
this section records deployment assumptions and must be treated as design
notes, not as evidence that those components are already implemented.



Go is available, so I can compile-check the client. Writing the three files now.I built both paths and compile-checked what I could. The Go client builds, vets, and passes a mock-server test of submit, state, and cancel. The job template renders in both modes and passes `bash -n`. I couldn't build the container or talk to a real `slurmrestd` here, so those two parts are unverified.

## Path 1: slurmrestd

`slurmrest/client.go` is a small Go client with a `Scheduler` interface (`Submit`, `State`, `Cancel`). That interface lets you write an `sbatch`/`sacct` CLI backend later and swap it in without touching the rest of the system.

**Things to check on your cluster:**
- **API version tracks the Slurm release.** Slurm 25.05 uses `v0.0.43`. Set `APIVersion` to match whatever your cluster runs.
- **Verify field names against your own spec.** Run `GET /openapi/v3` on your cluster and compare. The client sends the fields I'm most confident about (`nodes` as a string, `time_limit` as a `{number, set, infinite}` object, `job.script`). Anything else, such as `exclusive` or `signal`, goes through `JobSpec.Extra` once you've confirmed its shape.
- **Authentication.** The client sends `X-SLURM-USER-NAME` and `X-SLURM-USER-TOKEN`, with the JWT coming from a function so you can refresh it. AWS's docs use an `Authorization: Bearer` header, but that's their managed setup. Ask your admins which one your deployment accepts.
- **Don't rely on `#SBATCH` lines through REST.** I believe `slurmrestd` doesn't parse them, so set the equivalent job fields in `JobSpec`. The template's `#SBATCH` lines matter only for the CLI backend.
- **Finished jobs disappear.** `slurmctld` forgets them after about 5 minutes, so for older jobs query `slurmdb` (this needs accounting enabled).
- **If `slurmrestd` isn't enabled,** the CLI backend behind the same interface is your fallback, and many clusters will need it.

## Path 2: container

`container/openfoam.def` is an Apptainer definition built from the official OpenCFD image. The version is a build argument, and Docker Hub currently lists tags up to 2512. If AM PravaH uses the Foundation flavour (openfoam.org) instead, swap the `From:` line and `FOAM_BASHRC`; everything else stays the same.

**MPI decides whether multi-node containers perform well.** The stock image uses its own OpenMPI, which is unlikely to be tuned for your InfiniBand fabric or to match the host's PMIx. You have three options:

- **A (what the file does):** use the image's MPI with `srun --mpi=pmix`. It works only if the PMIx versions are compatible, so check with `ompi_info | grep -i pmix` inside the image.
- **B:** rebuild OpenFOAM inside the image against an MPI of the same family and major version as the host, plus the UCX/OFI libraries for your fabric. This is the fastest, but you maintain it.
- **C:** use the container only for preprocess and post, and run the multi-node solver on a host-installed OpenFOAM.

My suggestion is to start with A on one or two nodes, benchmark it against a bare-metal run of the same case, and move to B or C only if the gap is real. That comparison also makes a good result for your scaling report.

## One template, both modes

`templates/solver.sbatch.tmpl` switches on `.Container`. For the container mode, `FoamBashrc` must be the path inside the image (`/usr/lib/openfoam/openfoam<ver>/etc/bashrc`), not a host path. Both modes share the same checkpoint trap, so a USR1 warning before walltime produces a clean `writeNow` stop.

## Current implementation status

The repository now contains a runnable control-plane foundation organized as:

```text
cmd/foam-clutch/          CLI and HTTP health/metrics service
internal/manifest/        strict YAML manifest parsing and validation
internal/preflight/       case layout, symlink, and executable-directive checks
internal/stage/           SHA-256 content-addressed cache and run materialization
internal/store/           SQLite WAL-backed persistent job state
internal/service/         validate → stage → persist → submit orchestration
internal/supervisor/      Slurm state reconciliation
internal/telemetry/       Prometheus exposition metrics
client.go                 slurmrestd scheduler adapter
solver.sbatch.tmpl        batch template
openfoam.def              Apptainer definition
```

The implemented workflow is:

1. Load and strictly validate `foam-clutch/v1alpha1` YAML.
2. Validate the case directory and reject unsafe executable OpenFOAM directives
   and symlinks escaping the case.
3. Hash and copy the case into a content-addressed cache.
4. Materialize an isolated run directory so checkpoint edits never mutate the
   shared cache.
5. Persist the job in SQLite before submitting to Slurm.
6. Submit a solver script with `USR1` checkpoint handling and explicit
   `scontrol requeue`.
7. Expose job records and Prometheus metrics through the service.

The Slurm client provides:

- startup-time configuration validation through `NewClient`;
- HTTP and Unix-socket Slurm REST transport;
- context cancellation and bounded response reads;
- explicit validation for submitted job specifications;
- propagation of Slurm API errors returned in successful HTTP responses;
- injectable token providers for JWT refresh and a `Scheduler` interface for
  alternate scheduler adapters.

Run locally with:

```bash
go test ./...
go vet ./...
go run ./cmd/foam-clutch validate -manifest manifest.yaml
SLURM_REST_URL=http://slurmrestd:6820 \
SLURM_API_VERSION=v0.0.43 \
SLURM_USER="$USER" \
SLURM_JWT="$SLURM_JWT" \
go run ./cmd/foam-clutch submit -manifest manifest.yaml
go run ./cmd/foam-clutch serve -addr :8080 -db foam-clutch.db
```

Before production deployment, verify the exact Slurm OpenAPI version and field
names against the cluster, replace the example static JWT environment variable
with a refreshable token provider, add authentication
and authorization at the API boundary, and complete integration tests against
a staging `slurmrestd` instance. The current HTTP service intentionally exposes
health, metrics, and read-only job listing only; submission should be wired to
the authenticated service layer rather than made anonymous.
