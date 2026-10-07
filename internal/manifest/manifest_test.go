package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesRelativePaths(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(caseDir, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "manifest.yaml")
	data := []byte("apiVersion: foam-clutch/v1alpha1\nname: test\ncase:\n  path: ./case\nresources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\nsolver:\n  name: simpleFoam\n")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Case.Path != caseDir {
		t.Fatalf("case path = %q", m.Case.Path)
	}
}
