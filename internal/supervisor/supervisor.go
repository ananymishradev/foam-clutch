package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/ananymishradev/foam-clutch"
	"github.com/ananymishradev/foam-clutch/internal/store"
	"github.com/ananymishradev/foam-clutch/internal/telemetry"
)

type Supervisor struct {
	Scheduler    slurmrest.Scheduler
	Store        *store.Store
	Metrics      *telemetry.Metrics
	PollInterval time.Duration
	// MaxUnknown caps consecutive State errors before Watch gives up.
	// Slurm forgets finished jobs after MinJobAge (~5 min) and this cluster
	// has no accounting, so a job that vanished long ago can never resolve
	// to DONE/FAILED. Zero means the default (5).
	MaxUnknown int
}

func (s *Supervisor) maxUnknown() int {
	if s.MaxUnknown > 0 {
		return s.MaxUnknown
	}
	return 5
}

// slurmToLocal maps scheduler states to the local job state machine.
// Terminal states (DONE/FAILED/CANCELLED) stop Watch. REQUEUED-class states
// (TIMEOUT, PREEMPTED, NODE_FAIL, ...) keep polling: Slurm will requeue the
// job when --requeue is set and the solver asked for a writeNow checkpoint.
func slurmToLocal(state string) string {
	switch state {
	case "PENDING", "SUSPENDED":
		return "QUEUED"
	case "RUNNING", "COMPLETING":
		return "RUNNING"
	case "COMPLETED":
		return "DONE"
	case "FAILED", "OUT_OF_MEMORY":
		return "FAILED"
	case "CANCELLED":
		return "CANCELLED"
	case "TIMEOUT", "PREEMPTED", "NODE_FAIL", "REVOKED", "RESIZING":
		return "REQUEUED"
	default:
		return state
	}
}

func (s *Supervisor) Watch(ctx context.Context, localID string, slurmID int) error {
	if s.Scheduler == nil || s.Store == nil {
		return fmt.Errorf("supervisor requires scheduler and store")
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	// Poll once immediately so short jobs are not missed, then on a ticker.
	// Consecutive State errors (job gone from slurmctld, daemon unreachable)
	// mark the job UNKNOWN; after maxUnknown in a row Watch returns an error
	// instead of hanging forever. Callers watching from submit time with the
	// default 15s interval always observe the terminal state before Slurm
	// forgets the job (~MinJobAge).
	unknown := 0
	poll := func() (bool, error) {
		states, err := s.Scheduler.State(ctx, slurmID)
		if err != nil {
			unknown++
			_ = s.Store.UpdateState(ctx, localID, "UNKNOWN", err.Error(), slurmID)
			if unknown >= s.maxUnknown() {
				return false, fmt.Errorf("slurm job %d unobservable after %d polls: %w", slurmID, unknown, err)
			}
			return false, nil
		}
		unknown = 0
		if len(states) == 0 {
			return false, nil
		}
		local := slurmToLocal(states[0])
		if local == "REQUEUED" && s.Metrics != nil {
			s.Metrics.Requeues.Add(1)
		}
		_ = s.Store.UpdateState(ctx, localID, local, "", slurmID)
		if local == "DONE" || local == "FAILED" || local == "CANCELLED" {
			return true, nil
		}
		return false, nil
	}
	if done, err := poll(); err != nil {
		return err
	} else if done {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if done, err := poll(); err != nil {
				return err
			} else if done {
				return nil
			}
		}
	}
}
