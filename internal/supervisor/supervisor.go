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
}

func (s *Supervisor) Watch(ctx context.Context, localID string, slurmID int) error {
	if s.Scheduler == nil || s.Store == nil {
		return fmt.Errorf("supervisor requires scheduler and store")
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		states, err := s.Scheduler.State(ctx, slurmID)
		if err != nil {
			_ = s.Store.UpdateState(ctx, localID, "UNKNOWN", err.Error(), slurmID)
		} else if len(states) > 0 {
			state := states[0]
			local := map[string]string{"PENDING": "QUEUED", "RUNNING": "RUNNING", "COMPLETED": "DONE", "FAILED": "FAILED", "CANCELLED": "CANCELLED", "TIMEOUT": "REQUEUED"}[state]
			if local == "" {
				local = state
			}
			_ = s.Store.UpdateState(ctx, localID, local, "", slurmID)
			if local == "DONE" || local == "FAILED" || local == "CANCELLED" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
