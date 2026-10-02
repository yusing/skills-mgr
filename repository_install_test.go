package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRepositoryAcceptedAddresses(t *testing.T) {
	for _, test := range []struct {
		address string
		branch  string
		path    string
		clone   string
	}{
		{"https://github.com/owner/repo", "default", "", "https://github.com/owner/repo"},
		{"https://git.example.org/team/repo.git", "default", "", "https://git.example.org/team/repo.git"},
		{"https://git.example.org/skills.git", "default", "", "https://git.example.org/skills.git"},
		{"github.com/owner/repo", "default", "", "https://github.com/owner/repo"},
		{"owner/repo", "default", "", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo/tree/stable/skills/alpha", "stable", "skills/alpha/", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo/blob/stable/skills/alpha/SKILL.md", "stable", "skills/alpha/", "https://github.com/owner/repo"},
	} {
		t.Run(test.address, func(t *testing.T) {
			gitLog := fakeGit(t, map[string]map[string]gitTestFile{
				test.branch: {
					test.path + "SKILL.md": {contents: skillFile("alpha", "Repository alpha.", "body"), mode: 0o644},
				},
			})
			manager := newTestManager(t)
			project := t.TempDir()
			if err := manager.installRepository(t.Context(), project, test.address, "", io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			assertRepositoryInstalled(t, manager, project, "alpha", "body")
			logged, err := os.ReadFile(gitLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(logged), test.clone) {
				t.Fatalf("clone invocation = %q, want repository %q", logged, test.clone)
			}
		})
	}
}

func TestInstallRepositoryRootWinsAndCopiesOnlySkillResources(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"SKILL.md":             {contents: skillFile("alpha", "Repository alpha.", "root body"), mode: 0o644},
			"references/guide.md":  {contents: "guide", mode: 0o644},
			"scripts/run.sh":       {contents: "#!/bin/sh\necho alpha\n", mode: 0o755},
			"assets/logo.svg":      {contents: "<svg/>", mode: 0o644},
			"data/example.json":    {contents: "{}", mode: 0o644},
			"README.md":            {contents: "repository readme", mode: 0o644},
			"src/main.go":          {contents: "package unrelated", mode: 0o644},
			"skills/beta/SKILL.md": {contents: skillFile("beta", "Other skill.", "nested body"), mode: 0o644},
		},
	})
	manager := newTestManager(t)
	project := t.TempDir()
	if err := manager.installRepository(t.Context(), project, "owner/repo", "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	record := assertRepositoryInstalled(t, manager, project, "alpha", "root body")
	root, err := manager.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"references/guide.md": "guide", "scripts/run.sh": "#!/bin/sh\necho alpha\n",
		"assets/logo.svg": "<svg/>", "data/example.json": "{}",
	} {
		assertFile(t, filepath.Join(root, path), contents)
	}
	info, err := os.Stat(filepath.Join(root, "scripts/run.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable resource lost execute permission: info=%v, err=%v", info, err)
	}
	for _, path := range []string{"README.md", "src", "skills"} {
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unrelated repository path %q copied: %v", path, err)
		}
	}
}

func TestInstallRepositoryDiscoversAndSelectsFrontmatterName(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "only valid skill", true: "exact frontmatter name"}[explicit], func(t *testing.T) {
			files := map[string]gitTestFile{
				"collection/folder-not-name/SKILL.md":            {contents: skillFile("alpha", "Repository alpha.", "selected body"), mode: 0o644},
				"collection/folder-not-name/references/guide.md": {contents: "selected guide", mode: 0o644},
				"collection/invalid/SKILL.md":                    {contents: "not a skill manifest", mode: 0o644},
			}
			name := ""
			if explicit {
				files["other/beta/SKILL.md"] = gitTestFile{contents: skillFile("beta", "Other skill.", "other body"), mode: 0o644}
				name = "alpha"
			}
			fakeGit(t, map[string]map[string]gitTestFile{"default": files})
			manager := newTestManager(t)
			project := t.TempDir()
			if err := manager.installRepository(t.Context(), project, "owner/repo", name, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			record := assertRepositoryInstalled(t, manager, project, "alpha", "selected body")
			root, err := manager.remoteStore.contentRoot(record)
			if err != nil {
				t.Fatal(err)
			}
			assertFile(t, filepath.Join(root, "references/guide.md"), "selected guide")
			if _, err := os.Stat(filepath.Join(root, "other")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sibling repository content copied: %v", err)
			}
		})
	}
}

func TestInstallRepositoryAmbiguityDoesNotPersist(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{"default": {
		"one/SKILL.md": {contents: skillFile("alpha", "Alpha.", "one"), mode: 0o644},
		"two/SKILL.md": {contents: skillFile("beta", "Beta.", "two"), mode: 0o644},
	}})
	manager := newTestManager(t)
	project := t.TempDir()
	var output strings.Builder
	err := manager.installRepository(t.Context(), project, "owner/repo", "", &output, io.Discard)
	if err == nil {
		t.Fatal("ambiguous repository installed without choosing a skill")
	}
	reported := err.Error() + output.String()
	if !strings.Contains(reported, "alpha") || !strings.Contains(reported, "beta") {
		t.Fatalf("ambiguity omitted available names: %s", reported)
	}
	records, err := manager.remoteStore.records()
	if err != nil || len(records) != 0 {
		t.Fatalf("ambiguity persisted remote content: records=%v, err=%v", records, err)
	}
	for _, path := range []string{
		filepath.Join(project, lockName), filepath.Join(manager.paths.globalLockDir, lockName),
		filepath.Join(project, ".agents", "skills"), filepath.Join(project, ".claude", "skills"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ambiguity changed selection or placeholders at %s: %v", path, err)
		}
	}
}

func TestInstallRepositoryRepeatKeepsProjectAndGlobalEnabled(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "project", true: "global"}[global], func(t *testing.T) {
			fakeGit(t, map[string]map[string]gitTestFile{"default": {
				"SKILL.md": {contents: skillFile("alpha", "Repository alpha.", "body"), mode: 0o644},
			}})
			manager := newTestManager(t)
			manager.global = global
			project := t.TempDir()
			for range 2 {
				if err := manager.installRepository(t.Context(), project, "owner/repo", "", io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
				assertRepositoryInstalled(t, manager, project, "alpha", "body")
			}
			unselected := manager.paths.globalLockDir
			if global {
				unselected = project
			}
			selection, err := loadLock(unselected)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := selection.enabled("alpha"); exists {
				t.Fatal("install wrote the other selection scope")
			}
		})
	}
}

func TestInstallRepositorySyncRefetchesPersistedSubdirectory(t *testing.T) {
	gitLog := fakeGit(t, map[string]map[string]gitTestFile{"stable": {
		"skills/alpha/SKILL.md":            {contents: skillFile("alpha", "Repository alpha.", "body"), mode: 0o644},
		"skills/alpha/references/guide.md": {contents: "guide", mode: 0o644},
		"skills/beta/SKILL.md":             {contents: skillFile("beta", "Other skill.", "other"), mode: 0o644},
	}})
	manager := newTestManager(t)
	project := t.TempDir()
	if err := manager.installRepository(t.Context(), project, "https://github.com/owner/repo/tree/stable/skills/alpha", "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	record := assertRepositoryInstalled(t, manager, project, "alpha", "body")
	root, err := manager.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(root, "references/guide.md"), "guide")
	ageRemoteRecord(t, manager.remoteStore, record.ref())
	before := gitCloneCount(t, gitLog)
	if err := manager.sync(t.Context(), project, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := gitCloneCount(t, gitLog); got <= before {
		t.Fatalf("sync did not refetch stale repository content: before=%d, after=%d", before, got)
	}
	record = assertRepositoryInstalled(t, manager, project, "alpha", "body")
	root, err = manager.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(root, "references/guide.md"), "guide")
}

func TestInstallRepositoryRealGitAndFreshStoreSync(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable for real repository integration")
	}
	repository := t.TempDir()
	writeFile(t, filepath.Join(repository, "skills/alpha/SKILL.md"), skillFile("alpha", "Repository alpha.", "real git body"))
	writeFile(t, filepath.Join(repository, "skills/alpha/references/guide.md"), "real git guide\n")
	script := filepath.Join(repository, "skills/alpha/scripts/run.sh")
	writeFile(t, script, "#!/bin/sh\ncat references/guide.md\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repository, "skills/beta/SKILL.md"), skillFile("beta", "Other skill.", "unrelated"))
	config := filepath.Join(t.TempDir(), "gitconfig")
	fixtureURL := (&url.URL{Scheme: "file", Path: repository}).String()
	writeFile(t, config, fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/fixture/skills.git\n", fixtureURL))
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, arguments := range [][]string{
		{"init", "--initial-branch=stable"},
		{"add", "."},
		{"-c", "user.name=Repository Test", "-c", "user.email=repository-test@example.invalid", "commit", "-m", "Fixture"},
	} {
		command := exec.CommandContext(t.Context(), git, arguments...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v\n%s", arguments, err, output)
		}
	}
	manager := newTestManager(t)
	project := t.TempDir()
	if err := manager.installRepository(t.Context(), project, "https://github.com/fixture/skills/tree/stable/skills/alpha", "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	record := assertRepositoryInstalled(t, manager, project, "alpha", "real git body")
	assertInstalledScript := func(store *remoteSkillStore, record remoteSkillRecord) {
		t.Helper()
		root, err := store.contentRoot(record)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), filepath.Join(root, "scripts/run.sh"))
		command.Dir = root
		output, err := command.CombinedOutput()
		if err != nil || string(output) != "real git guide\n" {
			t.Fatalf("installed resource execution: output=%q, err=%v", output, err)
		}
		if _, err := os.Stat(filepath.Join(root, "skills/beta")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fresh clone copied unrelated skill: %v", err)
		}
	}
	assertInstalledScript(manager.remoteStore, record)

	// Transfer only the persisted selection, not the original content store.
	restored := newTestManager(t)
	restoredProject := t.TempDir()
	selection, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveLock(restoredProject, selection); err != nil {
		t.Fatal(err)
	}
	if err := restored.sync(t.Context(), restoredProject, io.Discard); err != nil {
		t.Fatal(err)
	}
	restoredRecord := assertRepositoryInstalled(t, restored, restoredProject, "alpha", "real git body")
	assertInstalledScript(restored.remoteStore, restoredRecord)
}

func assertRepositoryInstalled(t *testing.T, manager *manager, project, name, body string) remoteSkillRecord {
	t.Helper()
	selectionDir, placeholderBase := project, project
	if manager.global {
		selectionDir, placeholderBase = manager.paths.globalLockDir, manager.paths.placeholderDir
	}
	selection, err := loadLock(selectionDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, exists := selection.remote(name)
	if !exists || ref.Provider != repositoryProvider || repositoryProvider != "repository" || ref.Name != name || !hasBooleanSelection(selection, name, true) {
		t.Fatalf("repository install selection = %#v", selection)
	}
	for _, harness := range []string{".agents", ".claude"} {
		assertFile(t, filepath.Join(placeholderBase, harness, "skills", name, "SKILL.md"), wantPlaceholder(name, "Repository alpha."))
	}
	var output strings.Builder
	if err := manager.getContext(t.Context(), project, name, "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != body {
		t.Fatalf("installed body = %q, want %q", output.String(), body)
	}
	records, err := manager.remoteStore.records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ref() != ref {
		t.Fatalf("persisted repository records = %#v, want selection ref %#v", records, ref)
	}
	if manager.remoteContentProvider(ref.Provider) == nil {
		t.Fatal("repository content provider unavailable for persisted selection")
	}
	return records[0]
}
