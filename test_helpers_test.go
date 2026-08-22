package main

import (
	"fmt"
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
	home := filepath.Join(root, "home")
	managerHome := filepath.Join(home, ".skills-mgr")
	manager := &manager{paths: paths{
		userSkills:     filepath.Join(home, ".agents", "skills"),
		managedSkills:  filepath.Join(managerHome, "skills"),
		claudeSkills:   filepath.Join(home, ".claude", "skills"),
		grokSkills:     filepath.Join(home, ".grok", "skills"),
		codexHome:      filepath.Join(home, ".codex"),
		adminSkills:    filepath.Join(root, "etc", "codex", "skills"),
		managerHome:    managerHome,
		globalLockDir:  managerHome,
		legacyLockDir:  home,
		placeholderDir: home,
		selectionLocks: filepath.Join(cache, "selection-locks"),
		remoteRegistry: filepath.Join(cache, "skills-sh.json"),
		skillsMP:       filepath.Join(cache, "skillsmp.json"),
		remoteSkills:   filepath.Join(cache, "remote-skills"),
		daemonSocket:   filepath.Join(socketDir, "skills-mgr.sock"),
	}}
	for _, dir := range []string{manager.paths.globalLockDir, manager.paths.placeholderDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manager.remoteStore = newRemoteSkillStore(manager.paths.remoteSkills)
	return manager
}

// wantPlaceholder renders the stub content placeholders carry, so a change to
// placeholder frontmatter updates one place rather than every assertion.
func wantPlaceholder(name, description string) string {
	return fmt.Sprintf(
		"---\nname: %s\ndescription: %s\ndisable-model-invocation: true\n---\n",
		name,
		description,
	)
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
