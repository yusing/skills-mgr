package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInstallRepository(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "project", true: "global"}[global], func(t *testing.T) {
			fakeGit(t, map[string]map[string]gitTestFile{"main": {
				"skills/alpha/SKILL.md": {contents: skillFile("alpha", "Repository alpha.", "body"), mode: 0o644},
			}})
			taskHome := t.TempDir()
			t.Setenv("HOME", taskHome)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(taskHome, "cache"))
			t.Setenv("CODEX_HOME", filepath.Join(taskHome, ".codex"))
			project := t.TempDir()
			t.Chdir(project)
			stdoutReader, stdoutWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			stderrReader, stderrWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			previousStdout, previousStderr := os.Stdout, os.Stderr
			os.Stdout, os.Stderr = stdoutWriter, stderrWriter
			t.Cleanup(func() {
				os.Stdout, os.Stderr = previousStdout, previousStderr
				_ = stdoutReader.Close()
				_ = stdoutWriter.Close()
				_ = stderrReader.Close()
				_ = stderrWriter.Close()
			})
			args := []string{"install"}
			if global {
				args = append(args, "-g")
			}
			args = append(args, "https://github.com/owner/repo/tree/main/skills/alpha", "alpha")
			if err := run(args); err != nil {
				t.Fatal(err)
			}
			os.Stdout, os.Stderr = previousStdout, previousStderr
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
			stdout, err := io.ReadAll(stdoutReader)
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := io.ReadAll(stderrReader)
			if err != nil {
				t.Fatal(err)
			}
			layer := "project"
			if global {
				layer = "global"
			}
			if string(stdout) != "Installed alpha ("+layer+" selection)\n" || !strings.Contains(string(stderr), "Discovering repository skills") {
				t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
			}
			paths, err := defaultPaths()
			if err != nil {
				t.Fatal(err)
			}
			manager := &manager{paths: paths, global: global, remoteStore: newRemoteSkillStore(paths.remoteSkills, filepath.Join(paths.managedSkills, remoteSkillPatchDir))}
			assertRepositoryInstalled(t, manager, project, "alpha", "body")
		})
	}
}

func TestInstallRepositoryRejectsInvalidInputWithoutMutation(t *testing.T) {
	for _, address := range []string{
		"", "not-a-repo", "http://github.com/owner/repo", "git@github.com:owner/repo.git",
		"https://user:password@github.com/owner/repo", "https://github.com/owner/repo?token=secret",
		"https://github.com/owner/repo/tree/main/../alpha", "https://github.com/owner/repo/tree/main/%2e%2e/alpha",
		"https://github.com/owner/repo/tree/main/skills%2Falpha", "https://github.com/owner/repo/tree/-bad/alpha",
		"https://github.com/owner/repo/blob/main/README.md", "https://github.com/owner/repo/tree/main/%1bescape",
	} {
		t.Run(address, func(t *testing.T) {
			gitLog := fakeGit(t, nil)
			manager := newTestManager(t)
			project := t.TempDir()
			err := manager.installRepository(t.Context(), project, address, "", io.Discard, io.Discard)
			if err == nil {
				t.Fatal("invalid repository address accepted")
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error exposed credentials: %v", err)
			}
			if gitCloneCount(t, gitLog) != 0 {
				t.Fatal("invalid address triggered clone")
			}
			records, err := manager.remoteStore.records()
			if err != nil || len(records) != 0 {
				t.Fatalf("records = %v, err = %v", records, err)
			}
		})
	}
}

func TestInstallRepositoryEncodedBranch(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{"feature/skills": {
		"skills/alpha/SKILL.md": {contents: skillFile("alpha", "Repository alpha.", "body"), mode: 0o644},
	}})
	manager := newTestManager(t)
	project := t.TempDir()
	if err := manager.installRepository(t.Context(), project, "https://github.com/owner/repo.git/tree/feature%2Fskills/skills/alpha/", "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	record := assertRepositoryInstalled(t, manager, project, "alpha", "body")
	if record.Locator != "https://github.com/owner/repo/tree/feature%2Fskills/skills/alpha" {
		t.Fatalf("locator = %q", record.Locator)
	}
	ageRemoteRecord(t, manager.remoteStore, record.ref())
	if err := manager.sync(t.Context(), project, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRepositoryCancellationAndProgressFailure(t *testing.T) {
	for _, phase := range []string{"before clone", "after discovery", "before selection", "progress failure"} {
		t.Run(phase, func(t *testing.T) {
			fakeGit(t, map[string]map[string]gitTestFile{"default": {
				"SKILL.md": {contents: skillFile("alpha", "Repository alpha.", "body"), mode: 0o644},
			}})
			manager := newTestManager(t)
			project := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "before clone" {
				cancel()
			}
			progressFailure := errors.New("progress writer failed")
			progress := writerFunc(func(data []byte) (int, error) {
				if phase == "progress failure" {
					return 0, progressFailure
				}
				if phase == "after discovery" && strings.Contains(string(data), "Discovering") ||
					phase == "before selection" && strings.Contains(string(data), "Installing") {
					cancel()
				}
				return len(data), nil
			})
			err := manager.installRepository(ctx, project, "owner/repo", "", io.Discard, progress)
			if phase == "progress failure" {
				if err != nil {
					t.Fatalf("auxiliary progress error blocked installation: %v", err)
				}
				assertRepositoryInstalled(t, manager, project, "alpha", "body")
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want %v", err, context.Canceled)
			}
			records, err := manager.remoteStore.records()
			if err != nil || len(records) != 0 {
				t.Fatalf("failed installation persisted records: %v, err = %v", records, err)
			}
			if _, err := os.Stat(filepath.Join(project, lockName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed installation changed selection: %v", err)
			}
		})
	}
}
