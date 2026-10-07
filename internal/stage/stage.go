package stage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Result struct {
	Hash, Path string
	Cached     bool
}

func New(cacheDir string) (*Stager, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	return &Stager{cacheDir: cacheDir}, nil
}

type Stager struct{ cacheDir string }

func (s *Stager) Materialize(cachedPath, runDir string) error {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return err
	}
	return copyTree(cachedPath, runDir)
}

func (s *Stager) Stage(caseDir string) (Result, error) {
	hash, err := hashTree(caseDir)
	if err != nil {
		return Result{}, err
	}
	dest := filepath.Join(s.cacheDir, hash)
	if _, err := os.Stat(dest); err == nil {
		return Result{Hash: hash, Path: dest, Cached: true}, nil
	}
	tmp, err := os.MkdirTemp(s.cacheDir, ".stage-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmp)
	if err := copyCaseTree(caseDir, tmp); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, statErr := os.Stat(dest); statErr == nil {
			return Result{Hash: hash, Path: dest, Cached: true}, nil
		}
		return Result{}, err
	}
	return Result{Hash: hash, Path: dest}, nil
}

// caseTopDirs are the only OpenFOAM inputs that define a case. Everything
// else at the case root (README, manifests, *.sh, *.slurm, logs,
// .foam-cache, .foam-runs, processor*, .git) is documentation, artefacts, or
// runner state and must not affect the content hash or the cached copy.
var caseTopDirs = map[string]bool{"0": true, "constant": true, "system": true}

func includeRel(rel string) bool {
	if rel == "." {
		return false
	}
	top := rel
	if i := strings.Index(rel, string(os.PathSeparator)); i >= 0 {
		top = rel[:i]
	}
	return caseTopDirs[top]
}

func hashTree(root string) (string, error) {
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if !includeRel(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !includeRel(rel) {
			return nil
		}
		io.WriteString(h, rel+"\x00")
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(h, f)
		return err
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// copyCaseTree copies only 0/, constant/, system/ from src to dst.
func copyCaseTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}
		if !includeRel(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || strings.HasPrefix(filepath.Clean(filepath.Join(filepath.Dir(rel), link)), "..") {
				return fmt.Errorf("unsafe symlink %s", rel)
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(out, in)
		closeErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || strings.HasPrefix(filepath.Clean(filepath.Join(filepath.Dir(rel), link)), "..") {
				return fmt.Errorf("unsafe symlink %s", rel)
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(out, in)
		closeErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
}
