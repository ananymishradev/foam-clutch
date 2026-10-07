// Package slurmrest is a minimal slurmrestd client: submit, get, cancel.
//
// The REST schema changes between Slurm releases. Slurm 25.05 -> v0.0.43.
// Before trusting any field name below, diff against YOUR cluster's spec:
//
//	curl -s -H "X-SLURM-USER-NAME: $USER" -H "X-SLURM-USER-TOKEN: $SLURM_JWT" \
//	     http://<host>:6820/openapi/v3 | jq '.components.schemas'
//
// Anything not modelled here can be passed through JobSpec.Extra.
package slurmrest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Scheduler is what the rest of the runner depends on. Implement it once with
// this REST client and once with an sbatch/squeue/sacct CLI wrapper, and the
// control plane never needs to know which one is in use.
type Scheduler interface {
	Submit(ctx context.Context, spec JobSpec) (jobID int, err error)
	State(ctx context.Context, jobID int) (states []string, err error)
	Cancel(ctx context.Context, jobID int) error
}

type Config struct {
	// BaseURL like "http://127.0.0.1:6820". Ignored host-wise if SocketPath is set.
	BaseURL string
	// SocketPath, e.g. "/run/slurmrestd/slurmrestd.sock" (unix socket mode).
	SocketPath string
	// APIVersion, e.g. "v0.0.43" for Slurm 25.05.
	APIVersion string
	// User is the Slurm user the JWT was issued for.
	User string
	// Token returns a JWT. A func so you can refresh it (scontrol token ...).
	Token func(ctx context.Context) (string, error)
	// HTTPClient overrides the default client. Its Transport is replaced with
	// the Unix-socket transport when SocketPath is configured.
	HTTPClient *http.Client
}

type Client struct {
	cfg       Config
	http      *http.Client
	configErr error
}

func New(cfg Config) *Client {
	c, err := NewClient(cfg)
	if err != nil {
		return &Client{cfg: cfg, configErr: err}
	}
	return c
}

// NewClient validates cfg and constructs a client. Prefer this constructor in
// services so configuration failures are reported during startup.
func NewClient(cfg Config) (*Client, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	tr := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
	}
	if cfg.SocketPath != "" {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cfg.SocketPath)
		}
		cfg.BaseURL = "http://slurmrestd" // host is ignored over a unix socket
	}
	var hc *http.Client
	if cfg.HTTPClient == nil {
		hc = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	} else {
		copy := *cfg.HTTPClient
		hc = &copy
		if cfg.SocketPath != "" {
			hc.Transport = tr
		}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.BaseURL) == "" && strings.TrimSpace(cfg.SocketPath) == "" {
		return errors.New("slurmrestd base URL or socket path is required")
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("invalid slurmrestd base URL %q", cfg.BaseURL)
		}
		if u.User != nil {
			return errors.New("slurmrestd base URL must not contain credentials")
		}
	}
	if strings.TrimSpace(cfg.APIVersion) != cfg.APIVersion ||
		strings.TrimSpace(cfg.APIVersion) == "" ||
		strings.ContainsAny(cfg.APIVersion, `/\?#`) ||
		strings.Contains(cfg.APIVersion, "..") {
		return errors.New("valid Slurm API version is required")
	}
	if strings.TrimSpace(cfg.User) == "" {
		return errors.New("Slurm user is required")
	}
	if cfg.Token == nil {
		return errors.New("Slurm token provider is required")
	}
	if cfg.SocketPath != "" && strings.TrimSpace(cfg.SocketPath) != cfg.SocketPath {
		return errors.New("Slurm socket path must not have leading or trailing whitespace")
	}
	return nil
}

type JobSpec struct {
	Name         string
	Partition    string
	Account      string
	Dir          string // current_working_directory
	Stdout       string
	Stderr       string
	Dependency   string // e.g. "afterok:12345"
	Nodes        int
	TasksPerNode int
	TimeLimitMin int
	Requeue      bool
	Script       string
	Env          []string
	// Extra is merged into the "job" object last (wins on conflict). Use it for
	// version-specific fields such as exclusive/signal after checking /openapi/v3.
	Extra map[string]any
}

type apiMsg struct {
	Error       string `json:"error"`
	Description string `json:"description"`
}

func (m apiMsg) String() string {
	if m.Description != "" {
		return m.Error + ": " + m.Description
	}
	return m.Error
}

func (c *Client) url(path string) string {
	return fmt.Sprintf("%s/slurm/%s/%s", strings.TrimRight(c.cfg.BaseURL, "/"), c.cfg.APIVersion, path)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	if c.configErr != nil {
		return c.configErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return err
	}
	tok, err := c.cfg.Token(ctx)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	req.Header.Set("X-SLURM-USER-NAME", c.cfg.User)
	req.Header.Set("X-SLURM-USER-TOKEN", tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return fmt.Errorf("read %s response: %w", path, err)
	}
	if len(raw) > 8<<20 {
		return fmt.Errorf("slurmrestd %s response exceeds 8 MiB", path)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slurmrestd %s %s: %d: %s", method, path, resp.StatusCode, responseMessage(raw))
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("slurmrestd returned an empty response")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func responseMessage(raw []byte) string {
	var msg apiMsg
	if json.Unmarshal(raw, &msg) == nil && msg.String() != ":" {
		return msg.String()
	}
	return truncate(raw, 512)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

func (c *Client) Submit(ctx context.Context, s JobSpec) (int, error) {
	if err := validateJobSpec(s); err != nil {
		return 0, err
	}
	env := s.Env
	if len(env) == 0 {
		// slurmctld rejects submissions with no environment on recent versions.
		env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	}
	job := map[string]any{
		"name":                      s.Name,
		"partition":                 s.Partition,
		"nodes":                     strconv.Itoa(s.Nodes), // string in the v0.0.4x schema ("min-max")
		"tasks_per_node":            s.TasksPerNode,
		"tasks":                     s.Nodes * s.TasksPerNode,
		"current_working_directory": s.Dir,
		"environment":               env,
		"requeue":                   s.Requeue,
		"script":                    s.Script,
		// v0.0.4x wraps integers as {number,set,infinite}.
		"time_limit": map[string]any{"number": s.TimeLimitMin, "set": true, "infinite": false},
	}
	if s.Account != "" {
		job["account"] = s.Account
	}
	if s.Stdout != "" {
		job["standard_output"] = s.Stdout
	}
	if s.Stderr != "" {
		job["standard_error"] = s.Stderr
	}
	if s.Dependency != "" {
		job["dependency"] = s.Dependency
	}
	for k, v := range s.Extra {
		job[k] = v
	}

	var out struct {
		JobID  int      `json:"job_id"`
		Errors []apiMsg `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "job/submit", map[string]any{"job": job}, &out); err != nil {
		return 0, err
	}
	if len(out.Errors) > 0 {
		msgs := make([]string, len(out.Errors))
		for i, e := range out.Errors {
			msgs[i] = e.String()
		}
		return 0, errors.New("submit rejected: " + strings.Join(msgs, "; "))
	}
	if out.JobID == 0 {
		return 0, errors.New("submit returned no job_id")
	}
	return out.JobID, nil
}

func validateJobSpec(s JobSpec) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("job name is required")
	}
	if strings.ContainsAny(s.Name, "\r\n") {
		return errors.New("job name must not contain newlines")
	}
	if s.Nodes < 1 {
		return errors.New("job nodes must be greater than zero")
	}
	if s.TasksPerNode < 1 {
		return errors.New("tasks per node must be greater than zero")
	}
	if s.TimeLimitMin < 1 {
		return errors.New("time limit must be greater than zero")
	}
	if strings.TrimSpace(s.Script) == "" {
		return errors.New("job script is required")
	}
	if strings.ContainsAny(s.Dir, "\r\n") || strings.ContainsAny(s.Stdout, "\r\n") || strings.ContainsAny(s.Stderr, "\r\n") {
		return errors.New("job paths must not contain newlines")
	}
	return nil
}

// State returns the job_state array (e.g. ["RUNNING"], ["PENDING"], ["COMPLETED"]).
// slurmctld forgets finished jobs after MinJobAge (default 300 s). For anything
// older, query slurmdbd via GET /slurmdb/<version>/job/<id> (needs accounting).
func (c *Client) State(ctx context.Context, jobID int) ([]string, error) {
	if jobID < 1 {
		return nil, errors.New("job ID must be greater than zero")
	}
	var out struct {
		Jobs []struct {
			State []string `json:"job_state"`
		} `json:"jobs"`
		Errors []apiMsg `json:"errors"`
	}
	if err := c.do(ctx, http.MethodGet, "job/"+strconv.Itoa(jobID), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, errors.New("state query rejected: " + joinAPIErrors(out.Errors))
	}
	if len(out.Jobs) == 0 {
		return nil, fmt.Errorf("job %d not in slurmctld (finished long ago? query slurmdb)", jobID)
	}
	return out.Jobs[0].State, nil
}

func (c *Client) Cancel(ctx context.Context, jobID int) error {
	if jobID < 1 {
		return errors.New("job ID must be greater than zero")
	}
	var out struct {
		Errors []apiMsg `json:"errors"`
	}
	if err := c.do(ctx, http.MethodDelete, "job/"+strconv.Itoa(jobID), nil, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return errors.New("cancel rejected: " + joinAPIErrors(out.Errors))
	}
	return nil
}

func joinAPIErrors(errs []apiMsg) string {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.String()
	}
	return strings.Join(msgs, "; ")
}

var _ Scheduler = (*Client)(nil)
