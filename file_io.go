package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func readBoundedFile(file *os.File, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func writeAtomicFile(path, label string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	temporary, err := os.CreateTemp(directory, ".skills-mgr-")
	if err != nil {
		return fmt.Errorf("create %s: %w", label, err)
	}
	name := temporary.Name()
	defer os.Remove(name)

	_, err = temporary.Write(data)
	if err == nil {
		err = temporary.Chmod(0o600)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	return nil
}
