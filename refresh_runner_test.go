package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stubSkillsShRegistry(t *testing.T, manager *manager, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	manager.remote = &remoteRegistry{
		baseURL:   server.URL,
		cachePath: manager.paths.remoteRegistry,
		client:    server.Client(),
	}
}

func writeSkillsShSearch(t *testing.T, response http.ResponseWriter, skills []remoteSkill) {
	t.Helper()
	if err := json.NewEncoder(response).Encode(struct {
		Skills []remoteSkill `json:"skills"`
	}{Skills: skills}); err != nil {
		t.Errorf("encode skills.sh search: %v", err)
	}
}

func writeFreshRemoteCache(t *testing.T, manager *manager, updatedAt time.Time) {
	t.Helper()
	if err := saveRemoteCache(manager.paths.remoteRegistry, remoteRegistryCache{
		SchemaRevision: remoteRegistrySchemaRevision,
		UpdatedAt:      updatedAt.UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRunnerStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	manager := newTestManager(t)
	var hits atomic.Int32
	stubSkillsShRegistry(t, manager, func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
		t.Error("refresh ran after cancel")
	})
	if err := runRefreshRunner(ctx, manager, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("registry hits = %d, want 0", hits.Load())
	}
}

func TestRefreshRunnerUpdatesStaleRegistryCache(t *testing.T) {
	manager := newTestManager(t)
	writeFreshRemoteCache(t, manager, time.Now().Add(-remoteRefreshInterval-time.Second))
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/search" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		writeSkillsShSearch(t, response, []remoteSkill{{
			ID: "owner/repo/zeta", Name: "zeta", Source: "owner/repo", Installs: 8,
		}})
	})

	logger, logs := testLogger()
	if err := runRefreshRunner(t.Context(), manager, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `msg="refreshed registry cache" trigger=ondemand`) {
		t.Fatalf("log = %s", logs.String())
	}

	cache, err := loadRemoteCache(manager.paths.remoteRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.Topics) != len(skillsRegistryTopics) {
		t.Fatalf("topics = %d, want %d", len(cache.Topics), len(skillsRegistryTopics))
	}
	if len(cache.Topics[0].Skills) != 1 || cache.Topics[0].Skills[0].Name != "zeta" {
		t.Fatalf("cached skills = %#v", cache.Topics[0].Skills)
	}
}

func TestRefreshRunnerSkipsFreshRegistryCache(t *testing.T) {
	manager := newTestManager(t)
	writeFreshRemoteCache(t, manager, time.Now())
	var hits atomic.Int32
	stubSkillsShRegistry(t, manager, func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
		t.Error("refreshed a fresh registry cache")
	})
	logger, logs := testLogger()
	if err := runRefreshRunner(t.Context(), manager, logger); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("registry hits = %d, want 0", hits.Load())
	}
	if !strings.Contains(logs.String(), `msg="skipping registry cache refresh"`) {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestRefreshRunnerReportsRegistryFailure(t *testing.T) {
	manager := newTestManager(t)
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, `{"error":"unavailable","message":"try later"}`, http.StatusServiceUnavailable)
	})
	logger, logs := testLogger()
	if err := runRefreshRunner(t.Context(), manager, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "registry cache refresh failed") ||
		!strings.Contains(logs.String(), "try later") {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestRefreshRunnerUpdatesStaleRemoteSkills(t *testing.T) {
	t.Setenv("FAKE_GIT_ERROR", "")
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Alpha.", "body"),
				mode:     0o644,
			},
		},
	})
	manager := newTestManager(t)
	writeFreshRemoteCache(t, manager, time.Now())
	stubSkillsShRegistry(t, manager, func(http.ResponseWriter, *http.Request) {
		t.Error("registry refresh is not due")
	})
	manager.skillsMP = newSkillsMPRegistry("", "")
	alpha := remoteSkillRef{
		Provider: skillsShProvider, ID: "owner/repo/alpha",
		Name: "alpha", Locator: "owner/repo/alpha",
	}
	if _, err := manager.remoteStore.ensure(t.Context(), alpha, manager.remote); err != nil {
		t.Fatal(err)
	}
	ageRemoteRecord(t, manager.remoteStore, alpha)
	before := gitCloneCount(t, gitLog)
	logger, logs := testLogger()
	if err := runRefreshRunner(t.Context(), manager, logger); err != nil {
		t.Fatal(err)
	}
	if gitCloneCount(t, gitLog) != before+1 {
		t.Fatalf("clones = %d, want %d", gitCloneCount(t, gitLog), before+1)
	}
	if !strings.Contains(logs.String(), `msg="updated remote skill"`) {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestRefreshRunnerDoesNotFetchMissingGlobalRemote(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Alpha.", "body"),
				mode:     0o644,
			},
		},
	})
	manager := newTestManager(t)
	writeFreshRemoteCache(t, manager, time.Now())
	stubSkillsShRegistry(t, manager, func(http.ResponseWriter, *http.Request) {
		t.Error("registry refresh is not due")
	})
	manager.skillsMP = newSkillsMPRegistry("", "")
	alpha := remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
	}
	if err := saveLock(
		manager.paths.globalLockDir,
		testLock(
			map[string]bool{"alpha": true},
			nil,
			map[string]remoteSkillRef{"alpha": alpha},
		),
	); err != nil {
		t.Fatal(err)
	}
	if err := runRefreshRunner(t.Context(), manager, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	if gitCloneCount(t, gitLog) != 0 {
		t.Fatalf("clones = %d, want 0", gitCloneCount(t, gitLog))
	}
	records, err := manager.remoteStore.records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v", records)
	}
}

func TestRefreshRunnerSkipsWorkWhenLockHeld(t *testing.T) {
	manager := newTestManager(t)
	var hits atomic.Int32
	stubSkillsShRegistry(t, manager, func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
		t.Error("refreshed while lock held")
	})
	file, err := tryFlockExclusive(manager.paths.refreshLock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeExclusiveLock(file) })
	if err := runRefreshRunner(t.Context(), manager, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("registry hits = %d, want 0", hits.Load())
	}
}

func TestRefreshRunnerCommandWritesLog(t *testing.T) {
	manager := newTestManager(t)
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, _ *http.Request) {
		writeSkillsShSearch(t, response, []remoteSkill{{
			ID: "owner/repo/zeta", Name: "zeta", Source: "owner/repo", Installs: 8,
		}})
	})
	if err := runRefreshRunnerCommand(manager); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(manager.paths.refreshLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `msg="refreshed registry cache" trigger=ondemand`) {
		t.Fatalf("refresh log = %s", data)
	}
}

func TestRefreshRunnerCommandSilentWhenFresh(t *testing.T) {
	current := newTestManager(t)
	writeFreshRemoteCache(t, current, time.Now())
	stubSkillsShRegistry(t, current, func(http.ResponseWriter, *http.Request) {
		t.Error("refreshed a fresh registry cache")
	})
	if err := runRefreshRunnerCommand(current); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(current.paths.refreshLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("refresh log = %s", data)
	}
}

func TestMaybeStartRefreshRunnerLogsLockProbeError(t *testing.T) {
	restoreRefreshHooks(t)
	current := newTestManager(t)
	var started atomic.Int32
	startBackgroundRefresh = func(*manager) error {
		started.Add(1)
		return nil
	}
	if err := os.MkdirAll(current.paths.refreshLock, 0o700); err != nil {
		t.Fatal(err)
	}
	maybeStartRefreshRunner(current)
	if started.Load() != 0 {
		t.Fatalf("started = %d, want 0", started.Load())
	}
	data, err := os.ReadFile(current.paths.refreshLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `msg="start refresh runner"`) {
		t.Fatalf("refresh log = %s", data)
	}
}

func TestMaybeStartRefreshRunnerRespectsLock(t *testing.T) {
	restoreRefreshHooks(t)
	current := newTestManager(t)
	var started atomic.Int32
	startBackgroundRefresh = func(*manager) error {
		started.Add(1)
		return nil
	}
	file, err := tryFlockExclusive(current.paths.refreshLock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeExclusiveLock(file) })
	maybeStartRefreshRunner(current)
	if started.Load() != 0 {
		t.Fatalf("started = %d, want 0", started.Load())
	}
}

func TestMaybeStartRefreshRunnerStartsWhenLockIsFree(t *testing.T) {
	restoreRefreshHooks(t)
	current := newTestManager(t)
	var started atomic.Int32
	startBackgroundRefresh = func(*manager) error {
		started.Add(1)
		return nil
	}
	maybeStartRefreshRunner(current)
	if started.Load() != 1 {
		t.Fatalf("started = %d, want 1", started.Load())
	}
}

func TestRunStartsRefreshRunner(t *testing.T) {
	restoreRefreshHooks(t)
	var started atomic.Int32
	startBackgroundRefresh = func(*manager) error {
		started.Add(1)
		return nil
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = stdout
	t.Cleanup(func() {
		os.Stdout = originalStdout
		_ = stdout.Close()
	})
	if err := run([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 1 {
		t.Fatalf("started = %d, want 1", started.Load())
	}
}

func TestRefreshRunnerDispatchDoesNotSpawn(t *testing.T) {
	restoreRefreshHooks(t)
	var spawned, executed atomic.Int32
	startBackgroundRefresh = func(*manager) error {
		spawned.Add(1)
		return nil
	}
	executeRefreshRunner = func(*manager) error {
		executed.Add(1)
		return nil
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	if err := run([]string{refreshRunnerCommand}); err != nil {
		t.Fatal(err)
	}
	if spawned.Load() != 0 {
		t.Fatalf("spawned = %d, want 0", spawned.Load())
	}
	if executed.Load() != 1 {
		t.Fatalf("executed = %d, want 1", executed.Load())
	}
}

func TestUnknownDaemonCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	err := run([]string{"daemon"})
	if err == nil || err.Error() != `unknown command "daemon"` {
		t.Fatalf("error = %v", err)
	}
}

func TestTruncateRefreshLogIfNeeded(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "refresh.log")
		if err := truncateRefreshLogIfNeeded(path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected file to not exist, got err = %v", err)
		}
	})

	t.Run("under limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "refresh.log")
		content := strings.Repeat("log line\n", 100)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := truncateRefreshLogIfNeeded(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != content {
			t.Fatalf("content changed when under limit")
		}
	})

	t.Run("over limit truncates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "refresh.log")
		// Create a file larger than refreshLogMaxBytes
		lineCount := (refreshLogMaxBytes / 10) + 1000
		var builder strings.Builder
		for i := range lineCount {
			builder.WriteString("line ")
			builder.WriteString(strings.Repeat("X", 4))
			builder.WriteString(fmt.Sprintf(" %d\n", i))
		}
		content := builder.String()
		if len(content) <= refreshLogMaxBytes {
			t.Fatalf("test content too small: %d <= %d", len(content), refreshLogMaxBytes)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := truncateRefreshLogIfNeeded(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > refreshLogKeepBytes {
			t.Fatalf("truncated size = %d, want <= %d", len(data), refreshLogKeepBytes)
		}
		if len(data) == 0 {
			t.Fatal("truncated to empty file")
		}
		// Verify line boundary alignment - should not start with partial line
		if !strings.HasPrefix(string(data), "line ") {
			t.Fatalf("truncation broke line boundary, starts with: %q", string(data[:min(50, len(data))]))
		}
		// Verify we kept the most recent content (high line numbers)
		lastLine := strings.TrimSpace(string(data[strings.LastIndex(string(data[:len(data)-1]), "\n")+1:]))
		if !strings.HasPrefix(lastLine, "line ") || !strings.Contains(lastLine, fmt.Sprintf(" %d", lineCount-1)) {
			t.Fatalf("did not keep most recent content, last line = %q", lastLine)
		}
	})

	t.Run("preserves line boundaries", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "refresh.log")
		// Create content where truncation point lands mid-line
		var builder strings.Builder
		for range (refreshLogMaxBytes / 50) + 100 {
			builder.WriteString(strings.Repeat("ABCDEFGHIJ", 5))
			builder.WriteString("\n")
		}
		content := builder.String()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := truncateRefreshLogIfNeeded(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Every line should be complete (50 chars + newline)
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if line == "" && i == len(lines)-1 {
				continue // trailing newline
			}
			if len(line) != 50 {
				t.Fatalf("line %d has length %d, want 50", i, len(line))
			}
		}
	})
}

func TestOpenRefreshLogTruncatesOnOpen(t *testing.T) {
	manager := newTestManager(t)
	// Create an oversized log file
	var builder strings.Builder
	for i := range (refreshLogMaxBytes / 10) + 1000 {
		builder.WriteString(fmt.Sprintf("log entry %d\n", i))
	}
	if err := os.MkdirAll(filepath.Dir(manager.paths.refreshLog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.paths.refreshLog, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	originalSize, _ := os.Stat(manager.paths.refreshLog)
	if originalSize.Size() <= refreshLogMaxBytes {
		t.Fatalf("test file too small: %d", originalSize.Size())
	}

	file, err := openRefreshLog(manager.paths.refreshLog)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()

	info, err := os.Stat(manager.paths.refreshLog)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > refreshLogKeepBytes {
		t.Fatalf("log size after open = %d, want <= %d", info.Size(), refreshLogKeepBytes)
	}
}
