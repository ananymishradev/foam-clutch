package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Manifest struct {
	APIVersion string    `yaml:"apiVersion"`
	Name       string    `yaml:"name"`
	Case       Case      `yaml:"case"`
	Resources  Resources `yaml:"resources"`
	Solver     Solver    `yaml:"solver"`
	Storage    Storage   `yaml:"storage"`
}

type Case struct {
	Path string `yaml:"path"`
}

type Resources struct {
	Partition        string `yaml:"partition"`
	Account          string `yaml:"account"`
	Nodes            int    `yaml:"nodes"`
	TasksPerNode     int    `yaml:"tasksPerNode"`
	TimeLimitMinutes int    `yaml:"timeLimitMinutes"`
}

type Solver struct {
	Name              string   `yaml:"name"`
	Arguments         []string `yaml:"arguments"`
	Container         string   `yaml:"container"`
	CheckpointSeconds int      `yaml:"checkpointSeconds"`
}

type Storage struct {
	CacheDir string `yaml:"cacheDir"`
	RunDir   string `yaml:"runDir"`
}

func Load(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	base := filepath.Dir(path)
	if !filepath.IsAbs(m.Case.Path) {
		m.Case.Path = filepath.Join(base, m.Case.Path)
	}
	if m.Storage.CacheDir != "" && !filepath.IsAbs(m.Storage.CacheDir) {
		m.Storage.CacheDir = filepath.Join(base, m.Storage.CacheDir)
	}
	if m.Storage.RunDir != "" && !filepath.IsAbs(m.Storage.RunDir) {
		m.Storage.RunDir = filepath.Join(base, m.Storage.RunDir)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	var problems []string
	if m.APIVersion == "" {
		problems = append(problems, "apiVersion is required")
	}
	if m.APIVersion != "" && m.APIVersion != "foam-clutch/v1alpha1" {
		problems = append(problems, "apiVersion must be foam-clutch/v1alpha1")
	}
	if m.Name == "" {
		problems = append(problems, "name is required")
	}
	if filepath.IsAbs(m.Case.Path) == false || m.Case.Path == "." {
		problems = append(problems, "case.path must resolve to an absolute directory")
	}
	if m.Resources.Nodes < 1 {
		problems = append(problems, "resources.nodes must be greater than zero")
	}
	if m.Resources.TasksPerNode < 1 {
		problems = append(problems, "resources.tasksPerNode must be greater than zero")
	}
	if m.Resources.TimeLimitMinutes < 1 {
		problems = append(problems, "resources.timeLimitMinutes must be greater than zero")
	}
	if m.Solver.Name == "" {
		problems = append(problems, "solver.name is required")
	}
	if m.Solver.CheckpointSeconds < 0 {
		problems = append(problems, "solver.checkpointSeconds cannot be negative")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
