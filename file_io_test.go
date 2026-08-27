package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBoundedFileRejectsOversizedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if _, err := readBoundedFile(file, 4); err == nil ||
		!strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("bounded read error = %v", err)
	}
}

func TestWriteAtomicFileCommitsOrLeavesDestinationUntouched(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache", "data.json")
		if err := writeAtomicFile(path, "test cache", []byte("fresh\n")); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "fresh\n" {
			t.Fatalf("atomic file contents = %q", data)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("atomic file mode = %v", info.Mode().Perm())
		}
	})

	t.Run("failed commit", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "occupied")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(path, "keep")
		if err := os.WriteFile(keep, []byte("original\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := writeAtomicFile(path, "test cache", []byte("replacement\n")); err == nil {
			t.Fatal("atomic write replaced a non-empty directory")
		}
		data, err := os.ReadFile(keep)
		if err != nil {
			t.Fatalf("failed atomic write removed destination contents: %v", err)
		}
		if string(data) != "original\n" {
			t.Fatalf("failed atomic write changed destination contents to %q", data)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "occupied" {
			t.Fatalf("failed atomic write left temporary files: %v", entries)
		}
	})
}
