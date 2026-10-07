package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectsExecutableDirectives(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("#codeStream\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := (Runner{}).Run(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("expected preflight failure, got %v", err)
	}
}

func TestIgnoresDirectivesOutsideCaseDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("application simpleFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Docs and runner artefacts mention directive names but are not input.
	for _, p := range []string{"README.md", "run_openfoam.slurm", "manifest.yaml"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("#codeStream #calc codedFixedValue codedMixed libs\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (Runner{}).Run(context.Background(), root); err != nil {
		t.Fatalf("directives outside 0/constant/system must not fail preflight: %v", err)
	}
}

func TestSubdomainsParses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "system"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "system", "decomposeParDict"), []byte("numberOfSubdomains 8;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	n, err := Subdomains(root)
	if err != nil || n != 8 {
		t.Fatalf("Subdomains = %d, %v", n, err)
	}
}
