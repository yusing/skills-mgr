package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRemoteTogglePersistsOutsideAgentRootsAndReusesFreshContent(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/v1/skills/owner/repo/alpha" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"files": []map[string]string{
				{"path": "SKILL.md", "contents": skillFile("alpha", "Remote alpha.", "body")},
				{"path": "references/guide.md", "contents": "# Guide\n"},
			},
		})
	}))
	defer server.Close()

	manager := newTestManager(t)
	manager.remote = &remoteRegistry{baseURL: server.URL, client: server.Client()}
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}

	result, err := manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || requests != 1 || !result.RemoteSelected[ref.key()] {
		t.Fatalf("enable result = %#v, requests = %d", result, requests)
	}
	for _, forbidden := range []string{
		filepath.Join(project, ".agents", "skills"),
		filepath.Join(project, ".codex", "skills"),
		manager.paths.userSkills,
		filepath.Join(manager.paths.codexHome, "skills"),
	} {
		if _, err := os.Stat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remote toggle wrote forbidden root %s: %v", forbidden, err)
		}
	}
	var output strings.Builder
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "## alpha") {
		t.Fatalf("list omitted remote skill:\n%s", output.String())
	}
	output.Reset()
	if err := manager.get(project, "alpha", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "body" {
		t.Fatalf("remote skill body = %q", output.String())
	}

	result, err = manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if result.Enabled || requests != 1 {
		t.Fatalf("disable result = %#v, requests = %d", result, requests)
	}
	otherProject := t.TempDir()
	other, err := manager.selection(otherProject)
	if err != nil {
		t.Fatal(err)
	}
	if other["alpha"] {
		t.Fatal("remote toggle enabled another project")
	}

	result, err = manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || requests != 1 {
		t.Fatalf("fresh re-enable result = %#v, requests = %d", result, requests)
	}
}

func TestRemoteToggleRefreshesStaleContentWithoutReplacingOnFailure(t *testing.T) {
	requests := 0
	fail := false
	version := "one"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests++
		if fail {
			http.Error(response, `{"message":"offline"}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"files": []map[string]string{{
				"path":     "SKILL.md",
				"contents": skillFile("alpha", "Remote alpha.", version),
			}},
		})
	}))
	defer server.Close()

	manager := newTestManager(t)
	manager.remote = &remoteRegistry{baseURL: server.URL, client: server.Client()}
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	if _, err := manager.toggleRemote(t.Context(), project, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.toggleRemote(t.Context(), project, ref); err != nil {
		t.Fatal(err)
	}
	original := ageRemoteRecord(t, manager.remoteStore, ref)
	fail = true
	if _, err := manager.toggleRemote(t.Context(), project, ref); err == nil ||
		!strings.Contains(err.Error(), "offline") {
		t.Fatalf("stale enable error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests after failed refresh = %d, want 2", requests)
	}
	current := loadRemoteRecord(t, manager.remoteStore, ref)
	if current.Content != original.Content || !current.FetchedAt.Equal(original.FetchedAt) {
		t.Fatalf("failed refresh replaced metadata: %#v, want %#v", current, original)
	}
	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	if selected["alpha"] {
		t.Fatal("failed stale refresh enabled the skill")
	}

	fail = false
	version = "two"
	result, err := manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || requests != 3 {
		t.Fatalf("refreshed enable = %#v, requests = %d", result, requests)
	}
	var output strings.Builder
	if err := manager.get(project, "alpha", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "two" {
		t.Fatalf("refreshed skill body = %q", output.String())
	}
}

func TestSkillsMPRemoteToggleFetchesCompleteGitHubSkillInProcess(t *testing.T) {
	logPath := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
			"skills/alpha/scripts/check.sh": {
				contents: "#!/bin/sh\n",
				mode:     0o755,
			},
			"other.txt": {contents: "ignored", mode: 0o644},
		},
	})

	manager := newTestManager(t)
	manager.skillsMP = newSkillsMPRegistry("", "")
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
	}

	result, err := manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled ||
		!strings.Contains(string(logged), "clone --depth 1 --single-branch --branch main") {
		t.Fatalf("SkillsMP enable = %#v, git = %q", result, logged)
	}
	discovered, err := manager.findSkill(project, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(discovered.Root, "scripts", "check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("executable mode was not retained: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(discovered.Root, "other.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone copied repository file outside skill: %v", err)
	}
}

func TestSkillsMPRepositoryRootFindsRootSkillByDeclaredName(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
			"reference.md": {contents: "# Reference\n", mode: 0o644},
		},
	})
	files, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "SKILL.md" {
		t.Fatalf("root skill files = %#v", files)
	}
}

func TestSkillsMPRejectsOversizedSkill(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: strings.Repeat("x", remoteSkillMaxBytes+1),
				mode:     0o644,
			},
		},
	})
	_, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized checkout error = %v", err)
	}
}

func TestRemoteStoreRejectsUnsafeRefreshWithoutReplacingContent(t *testing.T) {
	provider := &staticRemoteProvider{files: []remoteSkillFile{{
		Path:     "SKILL.md",
		Contents: []byte(skillFile("alpha", "Alpha.", "safe")),
	}}}
	store := newRemoteSkillStore(filepath.Join(t.TempDir(), "remote-skills"))
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	_, err := store.ensure(t.Context(), ref, provider)
	if err != nil {
		t.Fatal(err)
	}
	record := ageRemoteRecord(t, store, ref)
	provider.files = []remoteSkillFile{
		{Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Alpha.", "unsafe"))},
		{Path: "../escape", Contents: []byte("escape")},
	}
	if _, err := store.ensure(t.Context(), ref, provider); err == nil ||
		!strings.Contains(err.Error(), "unsafe file path") {
		t.Fatalf("unsafe refresh error = %v", err)
	}
	current := loadRemoteRecord(t, store, ref)
	if current.Content != record.Content || !current.FetchedAt.Equal(record.FetchedAt) {
		t.Fatalf("unsafe refresh replaced record: %#v", current)
	}
	if _, err := os.Stat(filepath.Join(store.root, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe refresh wrote escaping file: %v", err)
	}
}

func TestRemoteStoreSerializesConflictingNamesAcrossInstances(t *testing.T) {
	root := filepath.Join(t.TempDir(), "remote-skills")
	stores := []*remoteSkillStore{
		newRemoteSkillStore(root),
		newRemoteSkillStore(root),
	}
	refs := []remoteSkillRef{
		{
			Provider: skillsShProvider, ID: "owner/repo/alpha",
			Name: "alpha", Locator: "owner/repo/alpha",
		},
		{
			Provider: skillsMPProvider, ID: "alpha-id",
			Name: "alpha", Locator: "https://github.com/owner/repo/tree/main/alpha",
		},
	}
	provider := &staticRemoteProvider{files: []remoteSkillFile{{
		Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Alpha.", "body")),
	}}}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range stores {
		wait.Go(func() {
			<-start
			_, err := stores[index].ensure(t.Context(), refs[index], provider)
			results <- err
		})
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	failures := 0
	for err := range results {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "already persisted") {
			failures++
		} else {
			t.Fatalf("unexpected concurrent persistence error: %v", err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent results = %d successes, %d conflicts", successes, failures)
	}
	records, err := stores[0].records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "alpha" {
		t.Fatalf("persisted records = %#v", records)
	}
}

func TestRemoteStoreLockHonorsCanceledContext(t *testing.T) {
	store := newRemoteSkillStore(filepath.Join(t.TempDir(), "remote-skills"))
	lock, err := store.lockExclusive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer closeRemoteStoreLock(lock)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ref := remoteSkillRef{
		Provider: skillsShProvider, ID: "owner/repo/alpha",
		Name: "alpha", Locator: "owner/repo/alpha",
	}
	_, err = store.ensure(ctx, ref, &staticRemoteProvider{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock wait error = %v", err)
	}
}

func TestRemoteRefreshRetainsPreviousGenerationForReaders(t *testing.T) {
	store := newRemoteSkillStore(filepath.Join(t.TempDir(), "remote-skills"))
	ref := remoteSkillRef{
		Provider: skillsShProvider, ID: "owner/repo/alpha",
		Name: "alpha", Locator: "owner/repo/alpha",
	}
	provider := &staticRemoteProvider{files: []remoteSkillFile{{
		Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Alpha.", "one")),
	}}}
	record, err := store.ensure(t.Context(), ref, provider)
	if err != nil {
		t.Fatal(err)
	}
	oldRoot, err := store.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	ageRemoteRecord(t, store, ref)
	provider.files[0].Contents = []byte(skillFile("alpha", "Alpha.", "two"))
	if _, err := store.ensure(t.Context(), ref, provider); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(oldRoot, "SKILL.md"))
	if err != nil {
		t.Fatalf("reader lost previous generation: %v", err)
	}
	if !strings.Contains(string(data), "one") {
		t.Fatalf("previous generation changed: %q", data)
	}
}

func TestRemoteFetchTTLStartsAtSuccessfulCompletion(t *testing.T) {
	store := newRemoteSkillStore(filepath.Join(t.TempDir(), "remote-skills"))
	ref := remoteSkillRef{
		Provider: skillsShProvider, ID: "owner/repo/alpha",
		Name: "alpha", Locator: "owner/repo/alpha",
	}
	provider := &staticRemoteProvider{files: []remoteSkillFile{{
		Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Alpha.", "body")),
	}}}
	started := time.Now()
	record, err := store.ensure(t.Context(), ref, provider)
	if err != nil {
		t.Fatal(err)
	}
	if record.FetchedAt.Before(started) {
		t.Fatalf("fetch completed at %s before it started at %s", record.FetchedAt, started)
	}
}

func TestDaemonRefreshesStaleRemoteSkillsAndRetainsFailures(t *testing.T) {
	skillsShRequests := 0
	skillsShFail := false
	skillsShServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		skillsShRequests++
		if skillsShFail {
			http.Error(response, `{"message":"still offline"}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"files": []map[string]string{{
				"path":     "SKILL.md",
				"contents": skillFile("alpha", "Alpha.", "body"),
			}},
		})
	}))
	defer skillsShServer.Close()

	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/beta/SKILL.md": {
				contents: skillFile("beta", "Beta.", "body"),
				mode:     0o644,
			},
		},
	})

	manager := newTestManager(t)
	manager.remote = &remoteRegistry{baseURL: skillsShServer.URL, client: skillsShServer.Client()}
	manager.skillsMP = newSkillsMPRegistry("", "")
	alpha := remoteSkillRef{
		Provider: skillsShProvider, ID: "owner/repo/alpha",
		Name: "alpha", Locator: "owner/repo/alpha",
	}
	beta := remoteSkillRef{
		Provider: skillsMPProvider, ID: "beta-id",
		Name: "beta", Locator: "https://github.com/owner/repo/tree/main/skills/beta",
	}
	if _, err := manager.remoteStore.ensure(
		t.Context(), alpha, manager.remote,
	); err != nil {
		t.Fatal(err)
	}
	ageRemoteRecord(t, manager.remoteStore, alpha)
	if _, err := manager.remoteStore.ensure(
		t.Context(), beta, manager.skillsMP,
	); err != nil {
		t.Fatal(err)
	}
	ageRemoteRecord(t, manager.remoteStore, beta)

	refreshPersistedRemoteSkills(t.Context(), manager, &strings.Builder{})
	logged, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	if skillsShRequests != 2 || strings.Count(string(logged), "\n") != 2 {
		t.Fatalf(
			"daemon requests = skills.sh %d, SkillsMP clones %d; want 2, 2",
			skillsShRequests,
			strings.Count(string(logged), "\n"),
		)
	}
	alphaRecord := ageRemoteRecord(t, manager.remoteStore, alpha)
	skillsShFail = true
	var diagnostic strings.Builder
	refreshPersistedRemoteSkills(t.Context(), manager, &diagnostic)
	if !strings.Contains(diagnostic.String(), "owner/repo/alpha") ||
		!strings.Contains(diagnostic.String(), "still offline") {
		t.Fatalf("daemon diagnostic = %q", diagnostic.String())
	}
	current := loadRemoteRecord(t, manager.remoteStore, alpha)
	if current.Content != alphaRecord.Content || !current.FetchedAt.Equal(alphaRecord.FetchedAt) {
		t.Fatalf("failed daemon refresh replaced alpha: %#v", current)
	}
}

func TestTUIRemoteSpaceToggleRefreshesInstalledAndCatalogState(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(response).Encode(map[string]any{
			"files": []map[string]string{{
				"path":     "SKILL.md",
				"contents": skillFile("alpha", "Alpha.", "body"),
			}},
		})
	}))
	defer server.Close()
	manager := newTestManager(t)
	manager.remote = &remoteRegistry{baseURL: server.URL, client: server.Client()}
	project := t.TempDir()
	current := model{
		manager: manager,
		project: project,
		tab:     remoteTab,
		remoteTopics: []remoteTopic{{
			Slug: "testing", Name: "Testing",
			Skills: []remoteSkill{{
				ID: "owner/repo/alpha", Name: "alpha", Source: "owner/repo",
			}},
		}},
		remoteCollapsed: map[string]bool{},
		remoteSelected:  map[string]bool{},
		selected:        map[string]bool{},
		cursor:          1,
		width:           60,
		height:          10,
	}

	updated, command := current.Update(tea.KeyMsg{Type: tea.KeySpace})
	current = updated.(model)
	if command == nil || !current.busy || current.status != "fetching alpha" ||
		!strings.Contains(current.View(), "Installing alpha") {
		t.Fatalf("remote enable did not start: %#v", current)
	}
	updated, _ = current.Update(command())
	current = updated.(model)
	if current.busy || current.status != "enabled alpha" ||
		!strings.Contains(current.View(), "alpha [enabled]") ||
		len(current.skills) != 1 {
		t.Fatalf("remote enable did not refresh model: %#v\n%s", current, current.View())
	}

	updated, command = current.Update(tea.KeyMsg{Type: tea.KeySpace})
	current = updated.(model)
	if command == nil || current.status != "disabling alpha" {
		t.Fatalf("remote disable did not start: %#v", current)
	}
	updated, _ = current.Update(command())
	current = updated.(model)
	if current.status != "disabled alpha" ||
		strings.Contains(current.View(), "alpha [enabled]") ||
		requests != 1 {
		t.Fatalf("remote disable = %#v, requests = %d", current, requests)
	}
}

func TestTUISkillsMPCloneUsesProgressPopup(t *testing.T) {
	manager := newTestManager(t)
	manager.skillsMP = newSkillsMPRegistry("", "")
	current := model{
		manager: manager,
		project: t.TempDir(),
		tab:     skillsMPTab,
		registrySkills: []registrySearchSkill{{
			ID:       "alpha-id",
			Name:     "alpha",
			Provider: skillsMPProvider,
			Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
		}},
		remoteSelected: map[string]bool{},
		selected:       map[string]bool{},
		width:          60,
		height:         10,
	}

	updated, command := current.Update(tea.KeyMsg{Type: tea.KeySpace})
	current = updated.(model)
	if command == nil || !current.busy ||
		!strings.Contains(current.View(), "Cloning with git --depth 1") {
		t.Fatalf("SkillsMP clone popup = %#v\n%s", current, current.View())
	}
}

func TestLocalToggleDoneReconcilesRemoteCatalogState(t *testing.T) {
	const key = "remote-key"
	current := model{
		skills: []discoveredSkill{{
			Name: "alpha", RemoteKey: key,
		}},
		selected:       map[string]bool{"alpha": true},
		remoteSelected: map[string]bool{key: true},
		busy:           true,
	}
	updated, _ := current.Update(toggleDone{skill: "alpha", enabled: false})
	got := updated.(model)
	if got.remoteSelected[key] {
		t.Fatalf("local toggle retained remote enabled state: %#v", got.remoteSelected)
	}
}

type staticRemoteProvider struct {
	files []remoteSkillFile
}

func (p *staticRemoteProvider) fetchSkill(
	context.Context,
	remoteSkillRef,
) ([]remoteSkillFile, error) {
	return p.files, nil
}

func ageRemoteRecord(
	t *testing.T,
	store *remoteSkillStore,
	ref remoteSkillRef,
) remoteSkillRecord {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	record, err := store.loadRecordLocked(ref.key())
	if err != nil {
		t.Fatal(err)
	}
	record.FetchedAt = time.Now().Add(-remoteSkillCacheTTL)
	if err := store.saveRecordLocked(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func loadRemoteRecord(
	t *testing.T,
	store *remoteSkillStore,
	ref remoteSkillRef,
) remoteSkillRecord {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	record, err := store.loadRecordLocked(ref.key())
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type gitTestFile struct {
	contents string
	mode     os.FileMode
}

func fakeGit(
	t *testing.T,
	branches map[string]map[string]gitTestFile,
) string {
	t.Helper()
	root := t.TempDir()
	for branch, files := range branches {
		writeGitTestFiles(t, filepath.Join(root, branch), files)
	}
	bin := t.TempDir()
	script := filepath.Join(bin, "git")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "-C" ]; then
  while IFS= read -r path; do
    printf '%s\0' "$path"
  done < "$2/.tracked"
  exit 0
fi
printf '%s\n' "$*" >> "$FAKE_GIT_LOG"
branch=default
previous=
for argument do
  if [ "$previous" = "--branch" ]; then branch=$argument; fi
  previous=$argument
  destination=$argument
done
source=$FAKE_GIT_ROOT/$branch
if [ ! -f "$source/.tracked" ]; then
  echo "remote branch $branch not found" >&2
  exit 1
fi
mkdir -p "$destination"
cp -R "$source/." "$destination/"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "git.log")
	t.Setenv("FAKE_GIT_ROOT", root)
	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func writeGitTestFiles(t *testing.T, root string, files map[string]gitTestFile) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for path, file := range files {
		destination := filepath.Join(root, filepath.FromSlash(path))
		writeFile(t, destination, file.contents)
		if err := os.Chmod(destination, file.mode); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	writeFile(t, filepath.Join(root, ".tracked"), strings.Join(paths, "\n")+"\n")
}
