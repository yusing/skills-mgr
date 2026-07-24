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
	lockName           = ".skills-mgr.json"
	lockSchemaRevision = 1
)

type lock struct {
	SchemaRevision int             `json:"schema_revision"`
	Skills         map[string]bool `json:"skills"`
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
	var result lock
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return lock{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return lock{}, fmt.Errorf("decode %s: unexpected data after lock", path)
	}
	if result.SchemaRevision != lockSchemaRevision {
		return lock{}, fmt.Errorf(
			"decode %s: unsupported schema revision %d; binary supports %d",
			path,
			result.SchemaRevision,
			lockSchemaRevision,
		)
	}
	if result.Skills == nil {
		return lock{}, fmt.Errorf("decode %s: missing skills", path)
	}
	return result, nil
}

func saveLock(project string, value lock) error {
	path := filepath.Join(project, lockName)
	value.SchemaRevision = lockSchemaRevision
	if value.Skills == nil {
		value.Skills = make(map[string]bool)
	}
	temp, err := os.CreateTemp(project, "."+lockName+"-")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	name := temp.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(value)
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
	}
}
