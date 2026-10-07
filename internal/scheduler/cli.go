package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	slurmrest "github.com/ananymishradev/foam-clutch"
)

// CliScheduler submits and polls via sbatch/squeue/scontrol/scancel.
// It implements slurmrest.Scheduler so the service never knows which
// backend is in use. Prefer it whenever slurmrestd is unavailable.
type CliScheduler struct {
	// Sbatch, Squeue, Scontrol, Scancel override binary names (tests).
	Sbatch   string
	Squeue   string
	Scontrol string
	Scancel  string
}

func (c *CliScheduler) bin(name, override string) string {
	if override != "" {
		return override
	}
	return name
}

func (c *CliScheduler) Submit(ctx context.Context, spec slurmrest.JobSpec) (int, error) {
	if err := validate(spec); err != nil {
		return 0, err
	}
	if strings.TrimSpace(spec.Dir) == "" {
		return 0, fmt.Errorf("job working directory is required")
	}
	if err := os.MkdirAll(spec.Dir, 0o750); err != nil {
		return 0, err
	}
	scriptPath := filepath.Join(spec.Dir, "foam-clutch.sbatch")
	if err := os.WriteFile(scriptPath, []byte(spec.Script), 0o750); err != nil {
		return 0, fmt.Errorf("write batch script: %w", err)
	}
	cmd := exec.CommandContext(ctx, c.bin("sbatch", c.Sbatch), "--parsable", scriptPath)
	cmd.Dir = spec.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("sbatch: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	idStr := strings.TrimSpace(stdout.String())
	// sbatch --parsable prints "12345" (optionally "12345;cluster").
	if i := strings.Index(idStr, ";"); i >= 0 {
		idStr = idStr[:i]
	}
	id, err := strconv.Atoi(strings.Fields(idStr)[0])
	if err != nil || id < 1 {
		return 0, fmt.Errorf("sbatch returned unparsable job id %q", idStr)
	}
	return id, nil
}

func (c *CliScheduler) State(ctx context.Context, jobID int) ([]string, error) {
	if jobID < 1 {
		return nil, fmt.Errorf("job ID must be greater than zero")
	}
	// squeue is the fast path for pending/running jobs.
	if out, err := exec.CommandContext(ctx, c.bin("squeue", c.Squeue),
		"-j", strconv.Itoa(jobID), "-h", "-o", "%T").CombinedOutput(); err == nil {
		if st := strings.TrimSpace(string(out)); st != "" {
			return []string{strings.Fields(st)[0]}, nil
		}
	}
	// scontrol still knows recently finished jobs (MinJobAge, ~5 min).
	out, err := exec.CommandContext(ctx, c.bin("scontrol", c.Scontrol),
		"show", "job", strconv.Itoa(jobID)).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("job %d not in slurmctld (finished long ago? query slurmdb/sacct)", jobID)
	}
	for _, field := range strings.Fields(string(out)) {
		if strings.HasPrefix(field, "JobState=") {
			st := strings.TrimPrefix(field, "JobState=")
			return []string{st}, nil
		}
	}
	return nil, fmt.Errorf("job %d state not found in scontrol output", jobID)
}

func (c *CliScheduler) Cancel(ctx context.Context, jobID int) error {
	if jobID < 1 {
		return fmt.Errorf("job ID must be greater than zero")
	}
	if out, err := exec.CommandContext(ctx, c.bin("scancel", c.Scancel),
		strconv.Itoa(jobID)).CombinedOutput(); err != nil {
		return fmt.Errorf("scancel: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func validate(s slurmrest.JobSpec) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("job name is required")
	}
	if s.Nodes < 1 || s.TasksPerNode < 1 || s.TimeLimitMin < 1 {
		return fmt.Errorf("nodes, tasksPerNode, and timeLimitMin must be positive")
	}
	if strings.TrimSpace(s.Script) == "" {
		return fmt.Errorf("job script is required")
	}
	return nil
}

var _ slurmrest.Scheduler = (*CliScheduler)(nil)
