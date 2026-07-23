package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestToggleAndSync(t *testing.T) {
	manager := newTestManager(t)
	project := filepath.Join(t.TempDir(), "project")
	skill := filepath.Join(manager.paths.library, "alpha", "SKILL.md")
	writeFile(t, skill, "v1")

	enabled, err := manager.toggle(project, "alpha")
	if err != nil || !enabled {
		t.Fatalf("enable = %v, %v", enabled, err)
	}
	installed := manager.installPath(project, "alpha")
	assertFile(t, filepath.Join(installed, "SKILL.md"), "v1")

	writeFile(t, skill, "v2")
	if _, err := manager.syncAll(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(installed, "SKILL.md"), "v2")

	enabled, err = manager.toggle(project, "alpha")
	if err != nil || enabled {
		t.Fatalf("disable = %v, %v", enabled, err)
	}
	if _, err := os.Lstat(installed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installed skill still exists: %v", err)
	}
}

func TestToggleRefusesUnmanagedDestination(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), "managed")
	destination := manager.installPath(project, "alpha")
	writeFile(t, filepath.Join(destination, "SKILL.md"), "local")

	if _, err := manager.toggle(project, "alpha"); err == nil {
		t.Fatal("toggle overwrote unmanaged destination")
	}
	assertFile(t, filepath.Join(destination, "SKILL.md"), "local")
}

func TestSyncRefusesReplacedDestination(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), "managed")
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}
	destination := manager.installPath(project, "alpha")
	if err := os.RemoveAll(destination); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destination, "SKILL.md"), "local")

	if _, err := manager.syncAll(); err == nil {
		t.Fatal("sync overwrote a replaced destination")
	}
	assertFile(t, filepath.Join(destination, "SKILL.md"), "local")
}

func TestInstallReplacesReservedMarkerWithoutFollowingIt(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	source := filepath.Join(manager.paths.library, "alpha")
	writeFile(t, filepath.Join(source, "SKILL.md"), "managed")
	target := filepath.Join(t.TempDir(), "target")
	writeFile(t, target, "unchanged")
	if err := os.Symlink(target, filepath.Join(source, ownershipMarker)); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "unchanged")
	assertFile(t, filepath.Join(manager.installPath(project, "alpha"), ownershipMarker), "alpha")
}

func TestDaemonUpdatesSelectedSkills(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := filepath.Join(manager.paths.library, "alpha", "SKILL.md")
	writeFile(t, skill, "v1")
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &signalWriter{ready: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, manager, output) }()
	select {
	case <-output.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not start")
	}

	writeFile(t, skill, "v2")
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			data, _ := os.ReadFile(filepath.Join(manager.installPath(project, "alpha"), "SKILL.md"))
			if string(data) == "v2" {
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				return
			}
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("daemon did not update skill; output: %s", output.String())
		}
	}
}

type signalWriter struct {
	bytes.Buffer
	ready chan struct{}
}

func (w *signalWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "Watching ") {
		select {
		case w.ready <- struct{}{}:
		default:
		}
	}
	return w.Buffer.Write(data)
}

func newTestManager(t *testing.T) *manager {
	t.Helper()
	root := t.TempDir()
	return &manager{paths: paths{
		library: filepath.Join(root, "data", "skills"),
		state:   filepath.Join(root, "state.json"),
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
