package preflight

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Finding struct{ Severity, Path, Message string }
type Report struct {
	Hash     string
	Findings []Finding
	Warnings []Finding
}
type Runner struct{ FoamDictionary, CheckMesh string }

func (r Runner) Run(ctx context.Context, caseDir string) (Report, error) {
	info, err := os.Stat(caseDir)
	if err != nil {
		return Report{}, fmt.Errorf("case: %w", err)
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("case %q is not a directory", caseDir)
	}
	for _, name := range []string{"system", "constant", "0"} {
		if st, err := os.Stat(filepath.Join(caseDir, name)); err != nil || !st.IsDir() {
			return Report{}, fmt.Errorf("case missing directory %s", name)
		}
	}
	report := Report{}
	err = filepath.WalkDir(caseDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(caseDir, path)
		if d.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			if !within(caseDir, target) {
				report.Findings = append(report.Findings, Finding{"error", rel, "symlink escapes case directory"})
			}
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "processor0") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			if err := scanFile(path, rel, &report); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Report{}, fmt.Errorf("scan case: %w", err)
	}
	if r.FoamDictionary != "" {
		runTool(ctx, r.FoamDictionary, caseDir, &report, "foamDictionary")
	}
	if r.CheckMesh != "" {
		runTool(ctx, r.CheckMesh, caseDir, &report, "checkMesh")
	}
	if len(report.Findings) > 0 {
		return report, fmt.Errorf("preflight failed with %d finding(s)", len(report.Findings))
	}
	return report, nil
}

func scanFile(path, rel string, report *Report) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for line := 1; s.Scan(); line++ {
		text := strings.TrimSpace(s.Text())
		for _, token := range []string{"#codeStream", "#calc", "codedFixedValue", "codedMixed", "libs"} {
			if strings.Contains(text, token) {
				report.Findings = append(report.Findings, Finding{"error", fmt.Sprintf("%s:%d", rel, line), "executable OpenFOAM directive " + token + " is not allowed"})
			}
		}
	}
	return s.Err()
}

func runTool(ctx context.Context, tool, dir string, report *Report, name string) {
	cmd := exec.CommandContext(ctx, tool, "-case", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		report.Findings = append(report.Findings, Finding{"error", name, fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))})
	}
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
