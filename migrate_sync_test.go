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
