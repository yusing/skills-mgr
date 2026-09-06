package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(data)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func testLogger() (*slog.Logger, *logBuffer) {
	logs := &logBuffer{}
	return slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), logs
}

func testLoggerSink() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func enabledCallHandler(project string) interp.CallHandlerFunc {
	return enabledCallHandlerWithEvidence(newProjectEvidenceIndex(project))
}

func ignoreRefreshRunner(*manager) error { return nil }

func TestMain(m *testing.M) {
	startBackgroundRefresh = ignoreRefreshRunner
	executeRefreshRunner = ignoreRefreshRunner
	os.Exit(m.Run())
}

func restoreRefreshHooks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		startBackgroundRefresh = ignoreRefreshRunner
		executeRefreshRunner = ignoreRefreshRunner
	})
}

func newTestManager(t *testing.T) *manager {
	t.Helper()
	root := t.TempDir()
	cache := filepath.Join(root, "cache", "skills-mgr")
	home := filepath.Join(root, "home")
	managerHome := filepath.Join(home, ".skills-mgr")
	manager := &manager{paths: paths{
		userSkills:     filepath.Join(home, ".agents", "skills"),
		managedSkills:  filepath.Join(managerHome, "skills"),
		claudeSkills:   filepath.Join(home, ".claude", "skills"),
		claudeSettings: filepath.Join(home, ".claude", "settings.json"),
		claudePlugins:  filepath.Join(home, ".claude", "plugins"),
		grokSkills:     filepath.Join(home, ".grok", "skills"),
		grokConfig:     filepath.Join(home, ".grok", "config.toml"),
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
		refreshLock:    filepath.Join(cache, "refresh.lock"),
		refreshLog:     filepath.Join(cache, "refresh.log"),
	}}
	for _, dir := range []string{manager.paths.globalLockDir, manager.paths.placeholderDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manager.remoteStore = newRemoteSkillStore(
		manager.paths.remoteSkills,
		filepath.Join(manager.paths.managedSkills, remoteSkillPatchDir),
	)
	return manager
}

func testLock(
	booleans map[string]bool,
	expressions map[string]string,
	remotes map[string]remoteSkillRef,
) lock {
	value := newLock()
	for name, enabled := range booleans {
		value.setEnabled(name, enabledValue{Boolean: new(enabled)})
	}
	for name, expression := range expressions {
		value.setEnabled(name, enabledValue{Expression: expression})
	}
	for name, ref := range remotes {
		value.setRemote(name, ref)
	}
	return value
}

func hasRemoteSelection(value lock, name string, enabled bool, ref remoteSkillRef) bool {
	currentEnabled, enabledExists := value.enabled(name)
	currentRemote, remoteExists := value.remote(name)
	return enabledExists &&
		currentEnabled.Boolean != nil &&
		*currentEnabled.Boolean == enabled &&
		remoteExists &&
		currentRemote == ref
}

func hasBooleanSelection(value lock, name string, enabled bool) bool {
	current, exists := value.enabled(name)
	return exists &&
		current.Boolean != nil &&
		*current.Boolean == enabled &&
		current.Expression == ""
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
