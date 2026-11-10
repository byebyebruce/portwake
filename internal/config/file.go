package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// LoadFile reads, parses and validates a YAML configuration file.
func LoadFile(path string) ([]Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	projects, err := UnmarshalYAML(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := Validate(projects); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return projects, nil
}

// SaveFile validates projects and atomically replaces path with their YAML
// form. If path is a symlink, its target is replaced. The existing file's
// permission bits are kept; a new file is created with 0644.
func SaveFile(path string, projects []Project) error {
	if err := Validate(projects); err != nil {
		return err
	}
	b, err := MarshalYAML(projects)
	if err != nil {
		return err
	}
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}
