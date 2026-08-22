package main

import (
	"os"
	"path/filepath"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

func enabledCallHandler(project string) interp.CallHandlerFunc {
	return enabledCallHandlerWithEvidence(newProjectEvidenceIndex(project))
}

func newTestManager(t *testing.T) *manager {
	t.Helper()
	root := t.TempDir()
	socketDir, err := os.MkdirTemp("", "smgr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove daemon socket directory: %v", err)
		}
	})
	cache := filepath.Join(root, "cache", "skills-mgr")
	manager := &manager{paths: paths{
		userSkills:     filepath.Join(root, "home", ".agents", "skills"),
		claudeSkills:   filepath.Join(root, "home", ".claude", "skills"),
		grokSkills:     filepath.Join(root, "home", ".grok", "skills"),
		codexHome:      filepath.Join(root, "home", ".codex"),
		adminSkills:    filepath.Join(root, "etc", "codex", "skills"),
		globalLockDir:  filepath.Join(root, "home"),
		selectionLocks: filepath.Join(cache, "selection-locks"),
		remoteRegistry: filepath.Join(cache, "skills-sh.json"),
		skillsMP:       filepath.Join(cache, "skillsmp.json"),
		remoteSkills:   filepath.Join(cache, "remote-skills"),
		daemonSocket:   filepath.Join(socketDir, "skills-mgr.sock"),
	}}
	if err := os.MkdirAll(manager.paths.globalLockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manager.remoteStore = newRemoteSkillStore(manager.paths.remoteSkills)
	return manager
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
