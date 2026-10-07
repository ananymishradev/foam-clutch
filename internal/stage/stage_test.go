package stage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageAndCacheReuse(t *testing.T) {
	root, cache, run := t.TempDir(), filepath.Join(t.TempDir(), "cache"), filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(filepath.Join(root, "field"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := New(cache)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Stage(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Stage(root)
	if err != nil {
		t.Fatal(err)
	}
	if second.Hash != first.Hash || !second.Cached {
		t.Fatalf("cache not reused: %#v %#v", first, second)
	}
	if err := s.Materialize(first.Path, run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run, "field")); err != nil {
		t.Fatal(err)
	}
}
