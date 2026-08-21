package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func startTestDaemon(t *testing.T, manager *manager) *logBuffer {
	t.Helper()
	logger, logs := testLogger()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runDaemonReady(ctx, manager, logger, ready)
	}()
	var receiveOnce sync.Once
	var runErr error
	recv := func() error {
		receiveOnce.Do(func() {
			runErr = <-done
		})
		return runErr
	}
	t.Cleanup(func() {
		cancel()
		if err := recv(); err != nil {
			t.Errorf("runDaemon = %v", err)
		}
	})
	select {
	case <-ready:
	case err := <-done:
		receiveOnce.Do(func() { runErr = err })
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("daemon exited before ready")
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	return logs
}

func waitLog(t *testing.T, logs *logBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log did not contain %q:\n%s", substr, logs.String())
}

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

func TestDaemonStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runDaemon(ctx, newTestManager(t), testLoggerSink()); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRefreshCommandUpdatesRegistryCache(t *testing.T) {
	manager := newTestManager(t)
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/search" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		writeSkillsShSearch(t, response, []remoteSkill{{
			ID: "owner/repo/zeta", Name: "zeta", Source: "owner/repo", Installs: 8,
		}})
	})

	logs := startTestDaemon(t, manager)
	if err := triggerDaemon(t.Context(), manager.paths.daemonSocket, daemonCommandRefresh); err != nil {
		t.Fatal(err)
	}
	waitLog(t, logs, `msg="received daemon command" command=refresh`)
	waitLog(t, logs, `msg="refreshed registry cache" trigger=command`)

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

func TestDaemonRefreshCommandReportsRegistryFailure(t *testing.T) {
	manager := newTestManager(t)
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, `{"error":"unavailable","message":"try later"}`, http.StatusServiceUnavailable)
	})
	logs := startTestDaemon(t, manager)
	err := triggerDaemon(t.Context(), manager.paths.daemonSocket, daemonCommandRefresh)
	if err == nil || !strings.Contains(err.Error(), "try later") {
		t.Fatalf("refresh error = %v", err)
	}
	waitLog(t, logs, "registry cache refresh failed")
}

func TestDaemonSyncCommandUpdatesStaleRemoteSkills(t *testing.T) {
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
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, _ *http.Request) {
		writeSkillsShSearch(t, response, nil)
	})
	manager.skillsMP = newSkillsMPRegistry("", "")
	alpha := remoteSkillRef{
		Provider: skillsShProvider, ID: "owner/repo/alpha",
		Name: "alpha", Locator: "owner/repo/alpha",
	}
	if _, err := manager.remoteStore.ensure(t.Context(), alpha, manager.remote); err != nil {
		t.Fatal(err)
	}

	logs := startTestDaemon(t, manager)
	waitLog(t, logs, `msg="persisted remote skill update finished" trigger=startup`)
	ageRemoteRecord(t, manager.remoteStore, alpha)
	before := gitCloneCount(t, gitLog)
	if err := triggerDaemon(t.Context(), manager.paths.daemonSocket, daemonCommandSync); err != nil {
		t.Fatal(err)
	}
	if gitCloneCount(t, gitLog) != before+1 {
		t.Fatalf("clones = %d, want %d", gitCloneCount(t, gitLog), before+1)
	}
	waitLog(t, logs, `msg="updated remote skill"`)
	waitLog(t, logs, `msg="received daemon command" command=sync`)
}

func TestDaemonCommandReportsWhenDaemonIsNotRunning(t *testing.T) {
	err := triggerDaemon(
		t.Context(),
		filepath.Join(t.TempDir(), "missing.sock"),
		daemonCommandRefresh,
	)
	if err == nil || err.Error() != "daemon is not running" {
		t.Fatalf("error = %v", err)
	}
}

func TestTriggerDaemonReturnsWhenContextIsCanceled(t *testing.T) {
	manager := newTestManager(t)
	listener, err := listenDaemonSocket(manager.paths.daemonSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close daemon listener: %v", err)
		}
	})

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- triggerDaemon(ctx, manager.paths.daemonSocket, daemonCommandRefresh)
	}()

	var conn net.Conn
	select {
	case conn = <-accepted:
		t.Cleanup(func() {
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close daemon connection: %v", err)
			}
		})
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for daemon connection")
	}
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("triggerDaemon error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("triggerDaemon did not return after context cancellation")
	}
}

func TestDaemonRejectsUnknownCommand(t *testing.T) {
	manager := newTestManager(t)
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, _ *http.Request) {
		writeSkillsShSearch(t, response, nil)
	})
	startTestDaemon(t, manager)
	err := triggerDaemon(t.Context(), manager.paths.daemonSocket, "status")
	if err == nil || !strings.Contains(err.Error(), `unknown command "status"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestDaemonRefusesSecondInstance(t *testing.T) {
	manager := newTestManager(t)
	stubSkillsShRegistry(t, manager, func(response http.ResponseWriter, _ *http.Request) {
		writeSkillsShSearch(t, response, nil)
	})
	startTestDaemon(t, manager)
	err := runDaemon(t.Context(), manager, testLoggerSink())
	if err == nil || err.Error() != "daemon already running" {
		t.Fatalf("error = %v", err)
	}
}
