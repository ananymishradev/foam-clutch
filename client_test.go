package slurmrest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmitStateCancel(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SLURM-USER-TOKEN") != "tok" {
			t.Errorf("missing token header")
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/slurm/v0.0.43/job/submit":
			json.NewDecoder(r.Body).Decode(&got)
			w.Write([]byte(`{"job_id":4242,"errors":[]}`))
		case r.Method == "GET" && r.URL.Path == "/slurm/v0.0.43/job/4242":
			w.Write([]byte(`{"jobs":[{"job_state":["RUNNING"]}]}`))
		case r.Method == "DELETE":
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := NewClient(Config{BaseURL: srv.URL, APIVersion: "v0.0.43", User: "u",
		Token: func(context.Context) (string, error) { return "tok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Submit(context.Background(), JobSpec{Name: "x", Nodes: 2, TasksPerNode: 32, TimeLimitMin: 60, Script: "#!/bin/bash\ntrue", Dependency: "afterok:1"})
	if err != nil || id != 4242 {
		t.Fatalf("submit: %v %d", err, id)
	}
	job := got["job"].(map[string]any)
	if job["nodes"] != "2" || job["tasks"].(float64) != 64 || job["dependency"] != "afterok:1" {
		t.Fatalf("bad body: %v", job)
	}
	st, err := c.State(context.Background(), id)
	if err != nil || st[0] != "RUNNING" {
		t.Fatalf("state: %v %v", err, st)
	}
	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientRejectsInvalidConfig(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("expected invalid config error")
	}
}

func TestSubmitValidatesJobSpecBeforeRequest(t *testing.T) {
	c, err := NewClient(Config{
		BaseURL: "http://127.0.0.1:6820", APIVersion: "v0.0.43", User: "u",
		Token: func(context.Context) (string, error) { return "tok", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(context.Background(), JobSpec{Name: "x"}); err == nil {
		t.Fatal("expected invalid job error")
	}
}

func TestAPIErrorsInSuccessfulResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"error":"InvalidJob","description":"job not found"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient(Config{BaseURL: srv.URL, APIVersion: "v0.0.43", User: "u",
		Token: func(context.Context) (string, error) { return "tok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background(), 1); err == nil {
		t.Fatal("expected API error")
	}
}
