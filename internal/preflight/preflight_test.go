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
