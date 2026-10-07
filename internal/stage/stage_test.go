package stage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageAndCacheReuse(t *testing.T) {
	root, cache, run := t.TempDir(), filepath.Join(t.TempDir(), "cache"), filepath.Join(t.TempDir(), "run")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("application simpleFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Auxiliary files at the case root must not affect the hash.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("#codeStream docs\n"), 0o640); err != nil {
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
	if _, err := os.Stat(filepath.Join(run, "system", "controlDict")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("auxiliary README.md must not be staged")
	}
}
