package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	lockName                 = ".skills-mgr.json"
	legacyLockSchemaRevision = 1
	lockSchemaRevision       = 2
)

type skillSelection struct {
	Enabled *bool           `json:"enabled,omitempty"`
	Remote  *remoteSkillRef `json:"remote,omitempty"`
}

type decodedLockFile struct {
	SchemaRevision int                        `json:"schema_revision"`
	Skills         map[string]*skillSelection `json:"skills"`
}

type lockFile struct {
	SchemaRevision int                       `json:"schema_revision"`
	Skills         map[string]skillSelection `json:"skills"`
}

type lock struct {
	SchemaRevision int
	Skills         map[string]bool
	Remote         map[string]remoteSkillRef
}

func loadLock(project string) (lock, error) {
	path := filepath.Join(project, lockName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newLock(), nil
	}
	if err != nil {
		return lock{}, fmt.Errorf("read %s: %w", path, err)
	}
	var revision struct {
		SchemaRevision int `json:"schema_revision"`
	}
	if err := json.Unmarshal(data, &revision); err != nil {
		return lock{}, fmt.Errorf("decode %s: %w", path, err)
	}

	result := newLock()
	switch revision.SchemaRevision {
	case legacyLockSchemaRevision:
		var legacy struct {
			SchemaRevision int             `json:"schema_revision"`
			Skills         map[string]bool `json:"skills"`
		}
		if err := decodeLockJSON(data, &legacy); err != nil {
			return lock{}, fmt.Errorf("decode %s: %w", path, err)
		}
		if legacy.Skills == nil {
			return lock{}, fmt.Errorf("decode %s: missing skills", path)
		}
		result.SchemaRevision = legacyLockSchemaRevision
		result.Skills = legacy.Skills
	case lockSchemaRevision:
		var current decodedLockFile
		if err := decodeLockJSON(data, &current); err != nil {
			return lock{}, fmt.Errorf("decode %s: %w", path, err)
		}
		if current.Skills == nil {
			return lock{}, fmt.Errorf("decode %s: missing skills", path)
		}
		for name, selection := range current.Skills {
			if selection == nil ||
				(selection.Enabled == nil && selection.Remote == nil) {
				return lock{}, fmt.Errorf(
					"decode %s: skill %q has neither enabled state nor remote metadata",
					path,
					name,
				)
			}
			if selection.Enabled != nil {
				result.Skills[name] = *selection.Enabled
			}
			if selection.Remote == nil {
				continue
			}
			if selection.Remote.Name != name {
				return lock{}, fmt.Errorf(
					"decode %s: skill %q remote name is %q",
					path,
					name,
					selection.Remote.Name,
				)
			}
			if err := selection.Remote.validate(); err != nil {
				return lock{}, fmt.Errorf(
					"decode %s: skill %q: %w",
					path,
					name,
					err,
				)
			}
			result.Remote[name] = *selection.Remote
		}
	default:
		return lock{}, fmt.Errorf(
			"decode %s: unsupported schema revision %d; binary supports %d",
			path,
			revision.SchemaRevision,
			lockSchemaRevision,
		)
	}
	return result, nil
}

func decodeLockJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected data after lock")
	}
	return nil
}

func saveLock(project string, value lock) error {
	path := filepath.Join(project, lockName)
	serialized := lockFile{
		SchemaRevision: lockSchemaRevision,
		Skills:         make(map[string]skillSelection, len(value.Skills)+len(value.Remote)),
	}
	for name, enabled := range value.Skills {
		serialized.Skills[name] = skillSelection{Enabled: new(enabled)}
	}
	for name, ref := range value.Remote {
		selection := serialized.Skills[name]
		selection.Remote = new(ref)
		serialized.Skills[name] = selection
	}
	temp, err := os.CreateTemp(project, "."+lockName+"-")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	name := temp.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(serialized)
	if err == nil {
		err = temp.Chmod(0o644)
	}
	err = errors.Join(err, temp.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func updateLock(project string, update func(*lock) (bool, error)) error {
	lockPath := filepath.Join(project, lockName+".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", lockPath, err)
	}
	defer file.Close()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)

	value, err := loadLock(project)
	if err != nil {
		return err
	}
	changed, err := update(&value)
	if err != nil || !changed {
		return err
	}
	return saveLock(project, value)
}

func newLock() lock {
	return lock{
		SchemaRevision: lockSchemaRevision,
		Skills:         make(map[string]bool),
		Remote:         make(map[string]remoteSkillRef),
	}
}
