package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateMovesSkills(t *testing.T) {
	manager := newTestManager(t)
	writeFile(t, filepath.Join(manager.paths.source, "alpha", "SKILL.md"), "alpha")

	count, err := manager.migrate()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migrate count = %d, want 1", count)
	}
	if _, err := os.Lstat(manager.paths.source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	assertFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), "alpha")

	if _, err := manager.migrate(); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
}

func TestMigrateRebasesExternalRelativeSymlink(t *testing.T) {
	manager := newTestManager(t)
	target := filepath.Join(filepath.Dir(filepath.Dir(manager.paths.source)), "skills", "alpha")
	writeFile(t, filepath.Join(target, "SKILL.md"), "alpha")
	link := filepath.Join(manager.paths.source, "alpha")
	relative, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.paths.source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, link); err != nil {
		t.Fatal(err)
	}

	count, err := manager.migrate()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migrate count = %d, want 1", count)
	}
	assertFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), "alpha")
	resolved, err := filepath.EvalSymlinks(filepath.Join(manager.paths.library, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("resolved link = %s, want %s", resolved, target)
	}
}

func TestMigratePreservesAbsoluteAndInternalSymlinks(t *testing.T) {
	manager := newTestManager(t)
	external := filepath.Join(t.TempDir(), "external")
	writeFile(t, filepath.Join(external, "SKILL.md"), "external")
	writeFile(t, filepath.Join(manager.paths.source, "target", "SKILL.md"), "internal")
	if err := os.Symlink(external, filepath.Join(manager.paths.source, "absolute")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(manager.paths.source, "internal")); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.migrate(); err != nil {
		t.Fatal(err)
	}
	absolute, err := os.Readlink(filepath.Join(manager.paths.library, "absolute"))
	if err != nil {
		t.Fatal(err)
	}
	if absolute != external {
		t.Fatalf("absolute link = %q, want %q", absolute, external)
	}
	assertFile(t, filepath.Join(manager.paths.library, "internal", "SKILL.md"), "internal")
}

func TestMigratePreservesResolutionAcrossIntermediateSymlink(t *testing.T) {
	manager := newTestManager(t)
	external := t.TempDir()
	target := filepath.Join(external, "skill")
	writeFile(t, filepath.Join(target, "SKILL.md"), "external")
	if err := os.Mkdir(filepath.Join(external, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.paths.source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "base"), filepath.Join(manager.paths.source, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alias/../skill", filepath.Join(manager.paths.source, "complex")); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.migrate(); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(manager.paths.library, "complex"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("resolved link = %s, want %s", resolved, target)
	}
	assertFile(t, filepath.Join(manager.paths.library, "complex", "SKILL.md"), "external")
}

func TestMigrateRebasesBrokenRelativeSymlinkWithoutTouchingUnrelatedFile(t *testing.T) {
	manager := newTestManager(t)
	external := t.TempDir()
	if err := os.Mkdir(filepath.Join(external, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.paths.source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "base"), filepath.Join(manager.paths.source, "alias")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(manager.paths.source, "missing")
	target := filepath.Join(external, "future")
	if err := os.Symlink("alias/../future", link); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(manager.paths.source, ".skill-mgr-link-existing")
	writeFile(t, unrelated, "keep")

	if _, err := manager.migrate(); err != nil {
		t.Fatal(err)
	}
	migrated := filepath.Join(manager.paths.library, "missing")
	rebased, err := os.Readlink(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Clean(filepath.Join(filepath.Dir(migrated), rebased)); got != target {
		t.Fatalf("broken link target = %s, want %s", got, target)
	}
	writeFile(t, filepath.Join(target, "SKILL.md"), "future")
	resolved, err := filepath.EvalSymlinks(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("resolved link = %s, want %s", resolved, target)
	}
	assertFile(t, filepath.Join(manager.paths.library, ".skill-mgr-link-existing"), "keep")
}

func TestMigrateDestinationCollisionLeavesRelativeSymlinkUnchanged(t *testing.T) {
	manager := newTestManager(t)
	target := filepath.Join(t.TempDir(), "target")
	link := filepath.Join(manager.paths.source, "alpha")
	if err := os.MkdirAll(manager.paths.source, 0o755); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, link); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(manager.paths.library, "existing"), "collision")

	if _, err := manager.migrate(); err == nil {
		t.Fatal("migrate accepted existing library")
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != relative {
		t.Fatalf("source link = %q, want %q", got, relative)
	}
	assertFile(t, filepath.Join(manager.paths.library, "existing"), "collision")
}

func TestRestoreLinksRestoresExactTargets(t *testing.T) {
	root := t.TempDir()
	links := []relativeLink{
		{path: filepath.Join(root, "one"), before: "../original-one"},
		{path: filepath.Join(root, "two"), before: "nested/../original-two"},
	}
	for _, link := range links {
		if err := os.Symlink("rewritten", link.path); err != nil {
			t.Fatal(err)
		}
	}

	if err := restoreLinks(links); err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		target, err := os.Readlink(link.path)
		if err != nil {
			t.Fatal(err)
		}
		if target != link.before {
			t.Fatalf("%s target = %q, want %q", link.path, target, link.before)
		}
	}
}

func newTestManager(t *testing.T) *manager {
	t.Helper()
	root := t.TempDir()
	return &manager{paths: paths{
		library: filepath.Join(root, "data", "skills"),
		source:  filepath.Join(root, "home", ".agents", "skills"),
	}}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
