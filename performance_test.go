package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func benchmarkCatalog(b *testing.B) (*manager, string) {
	b.Helper()
	project := b.TempDir()
	root := filepath.Join(b.TempDir(), "skills")
	m := &manager{paths: paths{userSkills: root, globalLockDir: b.TempDir()}}
	for i := range 100 {
		name := fmt.Sprintf("skill-%03d", i)
		directory := filepath.Join(root, name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			b.Fatal(err)
		}
		for file, contents := range map[string]string{
			skillManifestName: skillFile(name, "Benchmark skill.", "Instructions.\n"),
			"guide.md":        "Reference.\n",
		} {
			if err := os.WriteFile(filepath.Join(directory, file), []byte(contents), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	selected := make(map[string]bool, 100)
	for i := range 100 {
		selected[fmt.Sprintf("skill-%03d", i)] = true
	}
	if err := saveLock(project, testLock(selected, nil, nil)); err != nil {
		b.Fatal(err)
	}
	return m, project
}

func BenchmarkGet(b *testing.B) {
	m, project := benchmarkCatalog(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := m.getContext(b.Context(), project, "skill-050", "", io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkList(b *testing.B) {
	m, project := benchmarkCatalog(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := m.listContext(b.Context(), project, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
