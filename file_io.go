package main

import (
	"bytes"
	"encoding/json"
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

// writeAtomicJSONFile renders value as the indented JSON every on-disk metadata
// file in this package uses, then replaces path atomically.
func writeAtomicJSONFile(path, label string, value any) error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	return writeAtomicFile(path, label, data.Bytes())
}

// decodeStrictJSON owns the strict-decoding contract for every file this
// package reads back: unknown fields and trailing data are rejected so a
// corrupted or hand-edited file cannot be silently half-applied.
func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected data")
	}
	return nil
}
