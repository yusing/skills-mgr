package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type writerFunc func([]byte) (int, error)

func (write writerFunc) Write(data []byte) (int, error) {
	return write(data)
}

func TestRemoteToggleCreatesProjectPlaceholdersAndReusesFreshContent(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
			"skills/alpha/references/guide.md": {contents: "# Guide\n", mode: 0o644},
			"skills/unrelated/SKILL.md": {
				contents: skillFile("unrelated", "Unrelated.", "other"),
				mode:     0o644,
			},
		},
	})

	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	placeholder := "---\nname: alpha\ndescription: Remote alpha.\ndisable-model-invocation: true\n---\n"
	paths := []string{
		filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
		filepath.Join(project, ".claude", "skills", "alpha", "SKILL.md"),
	}

	result, err := manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || gitCloneCount(t, gitLog) != 1 || !result.RemoteSelected[ref.key()] {
		t.Fatalf("enable result = %#v, clones = %d", result, gitCloneCount(t, gitLog))
	}
	for _, path := range paths {
		assertFile(t, path, placeholder)
	}
	for _, forbidden := range []string{
		manager.paths.userSkills,
		filepath.Join(manager.paths.globalLockDir, ".claude", "skills"),
		filepath.Join(manager.paths.codexHome, "skills"),
	} {
		if _, err := os.Stat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("project remote toggle wrote global root %s: %v", forbidden, err)
		}
	}

	var output strings.Builder
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "<name>alpha</name>") {
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
	if result.Enabled || gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("disable result = %#v, clones = %d", result, gitCloneCount(t, gitLog))
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabled remote placeholder %s still exists: %v", path, err)
		}
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
	if !result.Enabled || gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("fresh re-enable result = %#v, clones = %d", result, gitCloneCount(t, gitLog))
	}
	for _, path := range paths {
		assertFile(t, path, placeholder)
	}
}

func TestGlobalRemoteToggleCreatesHomePlaceholders(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
		},
	})
	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
	manager.global = true
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	placeholder := "---\nname: alpha\ndescription: Remote alpha.\ndisable-model-invocation: true\n---\n"

	result, err := manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled {
		t.Fatalf("global enable result = %#v", result)
	}
	for _, path := range []string{
		filepath.Join(manager.paths.globalLockDir, ".agents", "skills", "alpha", "SKILL.md"),
		filepath.Join(manager.paths.globalLockDir, ".claude", "skills", "alpha", "SKILL.md"),
	} {
		assertFile(t, path, placeholder)
	}
	for _, root := range []string{
		filepath.Join(project, ".agents", "skills"),
		filepath.Join(project, ".claude", "skills"),
	} {
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("global remote toggle wrote project root %s: %v", root, err)
		}
	}
}

func TestRemotePlaceholdersDoNotOverwriteExistingSkill(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	existing := filepath.Join(project, ".claude", "skills", "alpha", "SKILL.md")
	writeFile(t, existing, "existing skill\n")

	err := manager.setRemotePlaceholders(project, "alpha", "Remote alpha.", true)
	if err == nil {
		t.Fatal("placeholder creation overwrote an existing skill")
	}
	assertFile(t, existing, "existing skill\n")
	if _, err := os.Stat(
		filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed placeholder creation left a partial skill: %v", err)
	}
}

func TestRemotePlaceholdersStoreOwnershipMarkerInSkillDirectory(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()

	if err := manager.setRemotePlaceholders(project, "alpha", "Remote alpha.", true); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{".agents", ".claude"} {
		assertFile(
			t,
			filepath.Join(project, root, "skills", "alpha", ".skills-mgr-placeholder"),
			"skills-mgr remote placeholder\n",
		)
		if _, err := os.Stat(
			filepath.Join(project, root, "skills", ".alpha.skills-mgr-placeholder"),
		); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete ownership marker exists under %s: %v", root, err)
		}
	}
}

func TestRemotePlaceholderCleanupRequiresOwnershipMarker(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	placeholder := filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md")
	content := "---\nname: alpha\ndisable-model-invocation: true\n---\n"
	writeFile(t, placeholder, content)

	if err := manager.setRemotePlaceholders(project, "alpha", "", false); err != nil {
		t.Fatal(err)
	}
	assertFile(t, placeholder, content)
}

func TestRemotePlaceholdersRejectSymlinkedRoots(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(project, ".agents")); err != nil {
		t.Fatal(err)
	}

	err := manager.setRemotePlaceholders(project, "alpha", "Remote alpha.", true)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlinked placeholder root error = %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target changed: entries = %v, error = %v", entries, err)
	}
}

func TestRemotePlaceholdersSkipCandidateAliases(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		target   string
		scope    string
		absolute bool
	}{
		{name: "Agents to Claude", source: ".agents", target: ".claude"},
		{name: "Agents to Codex", source: ".agents", target: ".codex"},
		{name: "Agents to Grok", source: ".agents", target: ".grok"},
		{name: "Claude to Agents", source: ".claude", target: ".agents"},
		{name: "Claude to Codex", source: ".claude", target: ".codex"},
		{name: "Claude to Grok", source: ".claude", target: ".grok"},
		{name: "Agents skills to Grok skills", source: ".agents", target: ".grok", scope: "skills"},
		{name: "Claude skills to Codex skills", source: ".claude", target: ".codex", scope: "skills"},
		{name: "Claude skills to Agents skills absolute", source: ".claude", target: ".agents", scope: "skills", absolute: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			target := filepath.Join(project, tt.target)
			source := filepath.Join(project, tt.source)
			linkTarget := tt.target
			if tt.scope == "skills" {
				target = filepath.Join(target, "skills")
				source = filepath.Join(source, "skills")
				linkTarget = filepath.Join("..", tt.target, "skills")
				if err := os.Mkdir(filepath.Dir(source), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.absolute {
				linkTarget = target
			}
			if err := os.Symlink(linkTarget, source); err != nil {
				t.Fatal(err)
			}

			if err := manager.setRemotePlaceholders(project, "alpha", "Remote alpha.", true); err != nil {
				t.Fatal(err)
			}
			otherManagedRoot := ".agents"
			if tt.source == ".agents" {
				otherManagedRoot = ".claude"
			}
			assertFile(
				t,
				filepath.Join(project, otherManagedRoot, "skills", "alpha", "SKILL.md"),
				"---\nname: alpha\ndescription: Remote alpha.\ndisable-model-invocation: true\n---\n",
			)
			if tt.target == ".agents" || tt.target == ".claude" {
				assertFile(
					t,
					filepath.Join(project, tt.target, "skills", "alpha", "SKILL.md"),
					"---\nname: alpha\ndescription: Remote alpha.\ndisable-model-invocation: true\n---\n",
				)
				return
			}
			if _, err := os.Stat(
				filepath.Join(project, tt.target, "skills", "alpha", "SKILL.md"),
			); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("alias target %s changed: %v", tt.target, err)
			}
		})
	}
}

func TestRemotePlaceholdersRejectInvalidCandidateAliases(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, project string)
	}{
		{
			name: "dangling target",
			setup: func(t *testing.T, project string) {
				t.Helper()
				if err := os.Symlink(".codex", filepath.Join(project, ".agents")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non-directory target",
			setup: func(t *testing.T, project string) {
				t.Helper()
				writeFile(t, filepath.Join(project, ".grok"), "not a directory\n")
				if err := os.Symlink(".grok", filepath.Join(project, ".claude")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			tt.setup(t, project)

			err := manager.setRemotePlaceholders(project, "alpha", "Remote alpha.", true)
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("invalid candidate alias error = %v", err)
			}
			if _, err := os.Stat(
				filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
			); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid candidate alias left an Agents placeholder: %v", err)
			}
		})
	}
}

func TestRemoteTogglePersistsIdentityWhenDisabled(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
		},
	})
	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
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
	projectLock, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if projectLock.Skills["alpha"] || projectLock.Remote["alpha"] != ref {
		t.Fatalf("disabled remote selection = %#v", projectLock)
	}

	data, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	var persisted lockFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	selection := persisted.Skills["alpha"]
	if selection.Enabled == nil || *selection.Enabled || selection.Remote == nil || *selection.Remote != ref {
		t.Fatalf("persisted remote selection = %#v", selection)
	}
}

func TestV2MigrationPersistsExplicitAndInheritedRemoteMetadata(t *testing.T) {
	manager := newTestManager(t)
	alpha := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	beta := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/beta",
		Name:     "beta",
		Locator:  "owner/repo/beta",
	}
	for _, ref := range []remoteSkillRef{alpha, beta} {
		if _, err := manager.remoteStore.ensure(
			t.Context(),
			ref,
			&staticRemoteProvider{files: []remoteSkillFile{{
				Path:     "SKILL.md",
				Contents: []byte(skillFile(ref.Name, "Remote skill.", "body")),
			}}},
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		writeFile(
			t,
			filepath.Join(manager.paths.userSkills, name, "SKILL.md"),
			skillFile(name, "Higher-precedence local skill.", "body"),
		)
	}
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.globalLockDir, lockName), `{
  "schema_revision": 1,
  "skills": {
    "alpha": true
  }
}`)
	writeFile(t, filepath.Join(project, lockName), `{
  "schema_revision": 1,
  "skills": {
    "beta": false
  }
}`)

	if _, err := manager.toggle(project, "local"); err != nil {
		t.Fatal(err)
	}
	projectLock, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, overridden := projectLock.Skills["alpha"]; overridden ||
		projectLock.Remote["alpha"] != alpha {
		t.Fatalf("inherited project selection = %#v", projectLock)
	}
	if projectLock.Skills["beta"] ||
		projectLock.Remote["beta"] != beta ||
		!projectLock.Skills["local"] {
		t.Fatalf("explicit project selections = %#v", projectLock)
	}

	data, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	var persisted lockFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	inherited := persisted.Skills["alpha"]
	if inherited.Enabled != nil ||
		inherited.Remote == nil ||
		*inherited.Remote != alpha {
		t.Fatalf("persisted inherited selection = %#v", inherited)
	}
	explicit := persisted.Skills["beta"]
	if explicit.Enabled == nil ||
		*explicit.Enabled ||
		explicit.Remote == nil ||
		*explicit.Remote != beta {
		t.Fatalf("persisted explicit selection = %#v", explicit)
	}

	manager.global = true
	if _, err := manager.toggle(project, "alpha", alpha.key()); err != nil {
		t.Fatal(err)
	}
	manager.global = false
	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	if selected["alpha"] {
		t.Fatalf("inherited selection did not follow global: %#v", selected)
	}
}

func TestInstalledRemoteToggleRestoresMissingIdentity(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	record, err := manager.remoteStore.ensure(
		t.Context(),
		ref,
		&staticRemoteProvider{files: []remoteSkillFile{{
			Path:     "SKILL.md",
			Contents: []byte(skillFile("alpha", "Remote alpha.", "body")),
		}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := record
	duplicate.Provider = skillsMPProvider
	duplicate.ID = "other-alpha-id"
	duplicate.Locator = "https://github.com/other/repo/tree/main/alpha"
	if err := manager.remoteStore.saveRecordLocked(duplicate); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}

	if err := manager.setRemotePlaceholders(project, "alpha", "Remote alpha.", true); err != nil {
		t.Fatal(err)
	}
	enabled, err := manager.toggle(project, "alpha", ref.key())
	if err != nil || enabled {
		t.Fatalf("disable installed remote = %v, %v", enabled, err)
	}
	got, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.Skills["alpha"] || got.Remote["alpha"] != ref {
		t.Fatalf("disabled installed remote selection = %#v", got)
	}
	for _, path := range []string{
		filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
		filepath.Join(project, ".claude", "skills", "alpha", "SKILL.md"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("installed remote disable left placeholder %s: %v", path, err)
		}
	}
	enabled, err = manager.toggle(project, "alpha", ref.key())
	if err != nil || !enabled {
		t.Fatalf("enable installed remote = %v, %v", enabled, err)
	}
	for _, path := range []string{
		filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
		filepath.Join(project, ".claude", "skills", "alpha", "SKILL.md"),
	} {
		assertFile(t, path, "---\nname: alpha\ndescription: Remote alpha.\ndisable-model-invocation: true\n---\n")
	}
}

func TestSyncRollsBackPlaceholdersWhenSelectionChanges(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	alpha := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	beta := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/beta",
		Name:     "beta",
		Locator:  "owner/repo/beta",
	}
	for _, ref := range []remoteSkillRef{alpha, beta} {
		if _, err := manager.remoteStore.ensure(
			t.Context(),
			ref,
			&staticRemoteProvider{files: []remoteSkillFile{{
				Path:     "SKILL.md",
				Contents: []byte(skillFile(ref.Name, "Remote skill.", "body")),
			}}},
		); err != nil {
			t.Fatal(err)
		}
		writeFile(
			t,
			filepath.Join(manager.paths.userSkills, ref.Name, "SKILL.md"),
			skillFile(ref.Name, "Higher-precedence local skill.", "body"),
		)
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": false, "beta": true},
		Remote: map[string]remoteSkillRef{"alpha": alpha, "beta": beta},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.setRemotePlaceholders(project, "alpha", "Remote skill.", true); err != nil {
		t.Fatal(err)
	}

	lockChanged := false
	output := writerFunc(func(data []byte) (int, error) {
		if lockChanged {
			return len(data), nil
		}
		current, err := loadLock(project)
		if err != nil {
			return 0, err
		}
		current.Skills["concurrent"] = true
		if err := saveLock(project, current); err != nil {
			return 0, err
		}
		lockChanged = true
		return len(data), nil
	})
	err := manager.sync(t.Context(), project, output)
	if err == nil || !strings.Contains(err.Error(), "project selection changed during sync") {
		t.Fatalf("concurrent sync error = %v", err)
	}
	if !lockChanged {
		t.Fatal("sync did not reach the concurrent lock change")
	}
	for _, root := range []string{".agents", ".claude"} {
		assertFile(
			t,
			filepath.Join(project, root, "skills", "alpha", "SKILL.md"),
			"---\nname: alpha\ndescription: Remote skill.\ndisable-model-invocation: true\n---\n",
		)
		if _, err := os.Stat(
			filepath.Join(project, root, "skills", "beta", "SKILL.md"),
		); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed sync left beta placeholder under %s: %v", root, err)
		}
	}
	current, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Skills["concurrent"] {
		t.Fatal("placeholder rollback overwrote the concurrent selection")
	}
}

func TestSyncBackfillsExplicitAndInheritedRemoteMetadata(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	alpha := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	beta := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/beta",
		Name:     "beta",
		Locator:  "owner/repo/beta",
	}
	for _, ref := range []remoteSkillRef{alpha, beta} {
		if _, err := manager.remoteStore.ensure(
			t.Context(),
			ref,
			&staticRemoteProvider{files: []remoteSkillFile{{
				Path:     "SKILL.md",
				Contents: []byte(skillFile(ref.Name, "Remote skill.", "body")),
			}}},
		); err != nil {
			t.Fatal(err)
		}
		writeFile(
			t,
			filepath.Join(manager.paths.userSkills, ref.Name, "SKILL.md"),
			skillFile(ref.Name, "Higher-precedence local skill.", "body"),
		)
	}
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"beta": false},
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.sync(t.Context(), project, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "alpha\n" {
		t.Fatalf("sync output = %q", output.String())
	}
	got, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, overridden := got.Skills["alpha"]; overridden ||
		got.Remote["alpha"] != alpha {
		t.Fatalf("inherited synchronized selection = %#v", got)
	}
	if got.Skills["beta"] || got.Remote["beta"] != beta {
		t.Fatalf("explicit synchronized selection = %#v", got)
	}
}

func TestCanceledSyncDoesNotPersistBackfilledMetadata(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	if _, err := manager.remoteStore.ensure(
		t.Context(),
		ref,
		&staticRemoteProvider{files: []remoteSkillFile{{
			Path:     "SKILL.md",
			Contents: []byte(skillFile("alpha", "Remote alpha.", "body")),
		}}},
	); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, newLock()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var output bytes.Buffer
	err = manager.sync(ctx, project, &output)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sync error = %v", err)
	}
	after, readErr := os.ReadFile(filepath.Join(project, lockName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("canceled sync persisted remote metadata")
	}
}

func TestSyncFetchesEnabledInheritedRemote(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
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
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, lock{
		Remote: map[string]remoteSkillRef{"alpha": ref},
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.sync(t.Context(), project, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "alpha\n" || gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("sync output = %q, clones = %d", output.String(), gitCloneCount(t, gitLog))
	}

	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": false},
	}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := manager.sync(t.Context(), project, &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 || gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("disabled sync output = %q, clones = %d", output.String(), gitCloneCount(t, gitLog))
	}
}

func TestSyncFetchesEnabledProjectRemotesAndReusesFreshContent(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
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
	disabled := remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "disabled-id",
		Name:     "disabled",
		Locator:  "https://github.com/owner/repo/tree/main/skills/disabled",
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{
			"alpha":    true,
			"disabled": false,
			"local":    true,
			"legacy":   true,
		},
		Remote: map[string]remoteSkillRef{
			"alpha":    ref,
			"disabled": disabled,
		},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.sync(t.Context(), project, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "alpha\n" || gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("sync output = %q, clones = %d", output.String(), gitCloneCount(t, gitLog))
	}
	for _, path := range []string{
		filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
		filepath.Join(project, ".claude", "skills", "alpha", "SKILL.md"),
	} {
		assertFile(t, path, "---\nname: alpha\ndescription: Remote alpha.\ndisable-model-invocation: true\n---\n")
	}
	output.Reset()
	if err := manager.sync(t.Context(), project, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "alpha\n" || gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("fresh sync output = %q, clones = %d", output.String(), gitCloneCount(t, gitLog))
	}
	after, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("sync changed project selection")
	}
	if _, err := manager.findSkill(project, "alpha"); err != nil {
		t.Fatalf("synchronized skill was not discoverable: %v", err)
	}
}

func TestSyncRejectsLocalNameCollisionWithoutFetching(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{})
	manager := newTestManager(t)
	manager.skillsMP = newSkillsMPRegistry("", "")
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"),
		skillFile("alpha", "Local alpha.", "body"),
	)
	ref := remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": true},
		Remote: map[string]remoteSkillRef{"alpha": ref},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err = manager.sync(t.Context(), project, &output)
	if err == nil || !strings.Contains(err.Error(), "already discovered") {
		t.Fatalf("sync collision error = %v", err)
	}
	if output.Len() != 0 || gitCloneCount(t, gitLog) != 0 {
		t.Fatalf("collision output = %q, clones = %d", output.String(), gitCloneCount(t, gitLog))
	}
	after, readErr := os.ReadFile(filepath.Join(project, lockName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed sync changed project selection")
	}
}

func TestSyncRejectsCachedLocatorMismatch(t *testing.T) {
	manager := newTestManager(t)
	oldRef := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	if _, err := manager.remoteStore.ensure(
		t.Context(),
		oldRef,
		&staticRemoteProvider{files: []remoteSkillFile{{
			Path:     "SKILL.md",
			Contents: []byte(skillFile("alpha", "Remote alpha.", "old")),
		}}},
	); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	newRef := oldRef
	newRef.Locator = "other/repo/alpha"
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": true},
		Remote: map[string]remoteSkillRef{"alpha": newRef},
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := manager.sync(t.Context(), project, &output)
	if err == nil || !strings.Contains(err.Error(), "identity conflicts") {
		t.Fatalf("locator mismatch error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("locator mismatch output = %q", output.String())
	}
	record := loadRemoteRecord(t, manager.remoteStore, oldRef)
	if record.Locator != oldRef.Locator {
		t.Fatalf("locator mismatch replaced record = %#v", record)
	}
}

func TestLocalToggleClearsStaleRemoteIdentity(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": false},
		Remote: map[string]remoteSkillRef{"alpha": ref},
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"),
		skillFile("alpha", "Local alpha.", "body"),
	)

	enabled, err := manager.toggle(project, "alpha")
	if err != nil || !enabled {
		t.Fatalf("enable local skill = %v, %v", enabled, err)
	}
	projectLock, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := projectLock.Remote["alpha"]; exists {
		t.Fatalf("local selection retained remote identity: %#v", projectLock.Remote)
	}
}

func TestRunSyncFetchesCommittedRemoteIdentity(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
		},
	})
	home := t.TempDir()
	cache := filepath.Join(home, "cache")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	project := t.TempDir()
	t.Chdir(project)
	ref := remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": true},
		Remote: map[string]remoteSkillRef{"alpha": ref},
	}); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"sync"}); err != nil {
		t.Fatal(err)
	}
	if gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("sync clones = %d, want 1", gitCloneCount(t, gitLog))
	}
	records, err := newRemoteSkillStore(
		filepath.Join(cache, "skills-mgr", "remote-skills"),
	).records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ref() != ref {
		t.Fatalf("synchronized records = %#v", records)
	}
}

func TestRemoteToggleRefreshesStaleContentWithoutReplacingOnFailure(t *testing.T) {
	t.Setenv("FAKE_GIT_ERROR", "")
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
		},
	})

	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
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
	t.Setenv("FAKE_GIT_ERROR", "offline")
	if _, err := manager.toggleRemote(t.Context(), project, ref); err == nil ||
		!strings.Contains(err.Error(), "offline") {
		t.Fatalf("stale enable error = %v", err)
	}
	if gitCloneCount(t, gitLog) != 2 {
		t.Fatalf("clones after failed refresh = %d, want 2", gitCloneCount(t, gitLog))
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

	writeFile(
		t,
		filepath.Join(
			os.Getenv("FAKE_GIT_ROOT"),
			"default",
			"skills",
			"alpha",
			"SKILL.md",
		),
		skillFile("alpha", "Remote alpha.", "two"),
	)
	t.Setenv("FAKE_GIT_ERROR", "")
	result, err := manager.toggleRemote(t.Context(), project, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || gitCloneCount(t, gitLog) != 3 {
		t.Fatalf("refreshed enable = %#v, clones = %d", result, gitCloneCount(t, gitLog))
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

func TestSkillsMPRemoteToggleLinksClaudeToAgentsAfterCloneAndRefresh(t *testing.T) {
	logPath := fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
			"skills/alpha/AGENTS.md": {contents: "first\n", mode: 0o644},
			"skills/alpha/CLAUDE.md": {link: "AGENTS.md"},
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

	if _, err := manager.toggleRemote(t.Context(), project, ref); err != nil {
		t.Fatal(err)
	}
	record := loadRemoteRecord(t, manager.remoteStore, ref)
	root, err := manager.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	assertClaudeAlias(t, root, "first\n")

	if _, err := manager.toggleRemote(t.Context(), project, ref); err != nil {
		t.Fatal(err)
	}
	ageRemoteRecord(t, manager.remoteStore, ref)
	writeFile(
		t,
		filepath.Join(os.Getenv("FAKE_GIT_ROOT"), "main", "skills", "alpha", "AGENTS.md"),
		"second\n",
	)
	if _, err := manager.toggleRemote(t.Context(), project, ref); err != nil {
		t.Fatal(err)
	}
	record = loadRemoteRecord(t, manager.remoteStore, ref)
	root, err = manager.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	assertClaudeAlias(t, root, "second\n")
	if clones := gitCloneCount(t, logPath); clones != 2 {
		t.Fatalf("clone count = %d, want 2", clones)
	}
}

func TestRemoteStoreAddsClaudeAliasWhenAgentsInstructionsExist(t *testing.T) {
	store := newRemoteSkillStore(filepath.Join(t.TempDir(), "remote-skills"))
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	record, err := store.ensure(t.Context(), ref, &staticRemoteProvider{files: []remoteSkillFile{
		{Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Alpha.", "body"))},
		{Path: "AGENTS.md", Contents: []byte("instructions\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	assertClaudeAlias(t, root, "instructions\n")
}

func TestRemoteStorePreservesDistinctClaudeInstructions(t *testing.T) {
	store := newRemoteSkillStore(filepath.Join(t.TempDir(), "remote-skills"))
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/alpha",
		Name:     "alpha",
		Locator:  "owner/repo/alpha",
	}
	record, err := store.ensure(t.Context(), ref, &staticRemoteProvider{files: []remoteSkillFile{
		{Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Alpha.", "body"))},
		{Path: "AGENTS.md", Contents: []byte("codex instructions\n")},
		{Path: "CLAUDE.md", Contents: []byte("claude instructions\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "CLAUDE.md")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("CLAUDE.md mode = %v, want regular file", info.Mode())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "claude instructions\n" {
		t.Fatalf("CLAUDE.md contents = %q", contents)
	}
}

func TestSkillsMPRejectsClaudeLinkOutsideAgentsInstructions(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"main": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
			"skills/alpha/AGENTS.md": {contents: "instructions\n", mode: 0o644},
			"skills/alpha/CLAUDE.md": {link: "../../outside"},
		},
	})
	_, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo/tree/main/skills/alpha",
	})
	if err == nil || !strings.Contains(err.Error(), `targets "../../outside"; want "AGENTS.md"`) {
		t.Fatalf("unsafe CLAUDE.md link error = %v", err)
	}
}

func TestSkillsMPRepositoryRootFetchesBareManifestFromOversizedRepository(t *testing.T) {
	filesByPath := map[string]gitTestFile{
		"SKILL.md": {
			contents: skillFile("alpha", "Remote alpha.", "body"),
			mode:     0o644,
		},
	}
	for index := range remoteSkillMaxFiles + 1 {
		filesByPath[fmt.Sprintf("src/file-%04d.txt", index)] = gitTestFile{
			contents: "unrelated",
			mode:     0o644,
		}
	}
	fakeGit(t, map[string]map[string]gitTestFile{"default": filesByPath})

	files, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "SKILL.md" {
		t.Fatalf("root skill files = %#v", files)
	}
}

func TestSkillsMPRepositoryRootFetchesFutureAgentSkillResources(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			".future-agent/skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Remote alpha.", "body"),
				mode:     0o644,
			},
			".future-agent/skills/alpha/assets/icon.svg": {
				contents: "<svg/>", mode: 0o644,
			},
			".future-agent/skills/alpha/data/styles.csv": {
				contents: "name,value\nminimal,1\n", mode: 0o644,
			},
			".future-agent/skills/alpha/references/guide.md": {
				contents: "# Guide\n", mode: 0o644,
			},
			".future-agent/skills/alpha/scripts/check.sh": {
				contents: "#!/bin/sh\n", mode: 0o755,
			},
			".future-agent/skills/alpha/notes.md": {
				contents: "not a skill resource", mode: 0o644,
			},
			".future-agent/skills/alpha/references-copy/ignored.md": {
				contents: "prefix collision", mode: 0o644,
			},
			".future-agent/skills/beta/assets/ignored.svg": {
				contents: "sibling collision", mode: 0o644,
			},
			".future-agent/skills/beta/SKILL.md": {
				contents: "malformed unrelated manifest", mode: 0o644,
			},
			"vendor/SKILL.md": {
				contents: "malformed unrelated manifest", mode: 0o644,
			},
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
	paths := make([]string, len(files))
	for index, file := range files {
		paths[index] = file.Path
	}
	want := []string{"SKILL.md", "assets/icon.svg", "data/styles.csv", "references/guide.md", "scripts/check.sh"}
	if !slices.Equal(paths, want) || files[4].Mode&0o111 == 0 {
		t.Fatalf("future agent skill files = %#v", files)
	}
}

func TestSkillsMPRepositoryRootRejectsExcessiveAgentSkillCandidates(t *testing.T) {
	filesByPath := make(map[string]gitTestFile, remoteSkillMaxFiles+1)
	for index := range remoteSkillMaxFiles + 1 {
		filesByPath[fmt.Sprintf(".agent-%04d/skills/alpha/SKILL.md", index)] = gitTestFile{
			contents: "candidate",
			mode:     0o644,
		}
	}
	fakeGit(t, map[string]map[string]gitTestFile{"default": filesByPath})

	_, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo",
	})
	if err == nil || !strings.Contains(err.Error(), "more than 1024 skill manifests") {
		t.Fatalf("excessive agent skill candidates error = %v", err)
	}
}

func TestSkillsMPRepositoryRootRejectsMissingManifest(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"default": {"README.md": {contents: "# Repository\n", mode: 0o644}},
	})
	_, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://github.com/owner/repo",
	})
	if err == nil || !strings.Contains(err.Error(), `does not contain skill "alpha"`) {
		t.Fatalf("missing root manifest error = %v", err)
	}
}

func TestSkillsMPFetchSkillRejectsMalformedURL(t *testing.T) {
	_, err := newSkillsMPRegistry("", "").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsMPProvider,
		ID:       "alpha-id",
		Name:     "alpha",
		Locator:  "https://example.com/owner/repo",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported SkillsMP GitHub URL") {
		t.Fatalf("malformed URL error = %v", err)
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
	t.Setenv("FAKE_GIT_ERROR", "")
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Alpha.", "body"),
				mode:     0o644,
			},
		},
		"main": {
			"skills/beta/SKILL.md": {
				contents: skillFile("beta", "Beta.", "body"),
				mode:     0o644,
			},
		},
	})

	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
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
	if strings.Count(string(logged), "\n") != 4 {
		t.Fatalf("daemon clones = %d, want 4", strings.Count(string(logged), "\n"))
	}
	alphaRecord := ageRemoteRecord(t, manager.remoteStore, alpha)
	t.Setenv("FAKE_GIT_ERROR", "still offline")
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
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"skills/alpha/SKILL.md": {
				contents: skillFile("alpha", "Alpha.", "body"),
				mode:     0o644,
			},
		},
	})
	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
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
	if command == nil || !current.busy || current.status != "installing alpha" ||
		!strings.Contains(current.View(), "Cloning with git --depth 1") {
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
		gitCloneCount(t, gitLog) != 1 {
		t.Fatalf("remote disable = %#v, clones = %d", current, gitCloneCount(t, gitLog))
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
	link     string
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
if [ -n "$FAKE_GIT_ERROR" ]; then
  printf '%s\n' "$FAKE_GIT_ERROR" >&2
  exit 1
fi
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

func gitCloneCount(t *testing.T, logPath string) int {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(logged), "\n")
}

func assertClaudeAlias(t *testing.T, root, wantContents string) {
	t.Helper()
	path := filepath.Join(root, "CLAUDE.md")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("CLAUDE.md mode = %v, want symlink", info.Mode())
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if target != "AGENTS.md" {
		t.Fatalf("CLAUDE.md target = %q, want AGENTS.md", target)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != wantContents {
		t.Fatalf("CLAUDE.md contents = %q, want %q", contents, wantContents)
	}
}

func writeGitTestFiles(t *testing.T, root string, files map[string]gitTestFile) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for path, file := range files {
		destination := filepath.Join(root, filepath.FromSlash(path))
		if file.link != "" {
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(file.link, destination); err != nil {
				t.Fatal(err)
			}
		} else {
			writeFile(t, destination, file.contents)
			if err := os.Chmod(destination, file.mode); err != nil {
				t.Fatal(err)
			}
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	writeFile(t, filepath.Join(root, ".tracked"), strings.Join(paths, "\n")+"\n")
}
