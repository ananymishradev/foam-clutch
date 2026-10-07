package store

import (
	"context"
	"testing"
	"time"
)

func TestPersistsJobs(t *testing.T) {
	s, err := Open(t.TempDir() + "/jobs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.Create(context.Background(), Job{ID: "id", Name: "case", State: "STAGED", ManifestPath: "manifest.yaml", CaseHash: "hash", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateState(context.Background(), "id", "QUEUED", "", 42); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get(context.Background(), "id")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != "QUEUED" || j.SlurmID != 42 {
		t.Fatalf("unexpected job %#v", j)
	}
}
