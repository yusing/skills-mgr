package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestScriptCommandCancellation(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range []string{".sh", ".py", ".js"} {
		t.Run(extension, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			root := filepath.Join(project, ".agents", "skills", "demo")
			writeFile(t, filepath.Join(root, skillManifestName), skillFile("demo", "Demo.", "body\n"))
			program := "#!/bin/sh\nprintf 'ready\\n'\nexec " + sleep + " 30\n"
			path := filepath.Join(root, "script"+extension)
			writeFile(t, path, program)
			if extension == ".sh" {
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				// Test interpreter selection without depending on installed runtimes.
				bin := t.TempDir()
				name := "python3"
				if extension == ".js" {
					name = "node"
				}
				writeExecutable(t, filepath.Join(bin, name), program)
				t.Setenv("PATH", bin)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command, err := manager.scriptCommandContext(ctx, project, "demo/script"+extension, nil)
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			line, err := bufio.NewReader(output).ReadString('\n')
			if err != nil || line != "ready\n" {
				t.Fatalf("script readiness = %q, %v", line, err)
			}
			cancel()
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled script succeeded")
				}
			case <-time.After(2 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Fatal("script did not terminate after cancellation")
			}
		})
	}
}

func TestScriptCommandCanceledBeforeStart(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(project, ".agents", "skills", "demo")
	writeFile(t, filepath.Join(root, skillManifestName), skillFile("demo", "Demo.", ""))
	bin := t.TempDir()
	for _, name := range []string{"python3", "node"} {
		writeExecutable(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", bin)
	for _, extension := range []string{".sh", ".py", ".js", ".txt"} {
		writeFile(t, filepath.Join(root, "script"+extension), "#!/bin/sh\nexit 0\n")
		if extension == ".sh" {
			if err := os.Chmod(filepath.Join(root, "script"+extension), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		command, err := manager.scriptCommandContext(ctx, project, "demo/script"+extension, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Run(); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s run error = %v, want cancellation", extension, err)
		}
	}
}

func TestRegistryInstallCancellationWaitsForCompletion(t *testing.T) {
	for _, tab := range []int{remoteTab, skillsMPTab} {
		manager := newTestManager(t)
		current := model{
			manager: manager, project: t.TempDir(), tab: tab,
			filterQuery: "demo", registrySkills: []registrySearchSkill{{
				ID: "owner/repo/demo", Name: "demo", Provider: skillsShProvider, Locator: "owner/repo/demo",
			}},
		}
		next, install := toggleSelectedSkill(current)
		busy := next.(model)
		if install == nil || !busy.busy || busy.busyCancel == nil {
			t.Fatal("installation was not scheduled with cancellation")
		}
		next, quit := updateKey(busy, tea.KeyMsg{Type: tea.KeyCtrlC})
		canceling := next.(model)
		if quit != nil || !canceling.busy || !canceling.quitAfterBusy {
			t.Fatal("Ctrl-C did not wait for installation cleanup")
		}
		result := install().(remoteToggleDone)
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("installation error = %v, want cancellation", result.err)
		}
		next, quit = canceling.Update(result)
		finished := next.(model)
		if finished.busy || finished.busyCancel != nil || finished.progressTitle != "" || finished.progressDetail != "" {
			t.Fatal("installation completion retained busy state")
		}
		if quit == nil {
			t.Fatal("completion did not honor pending quit")
		}
		if _, ok := quit().(tea.QuitMsg); !ok {
			t.Fatal("completion did not return quit")
		}
		selection, err := loadLock(current.project)
		if err != nil || len(selection.Skills) != 0 {
			t.Fatalf("canceled installation changed selection: %#v, %v", selection, err)
		}
	}
}

func TestRegistryToggleCompletionClearsCancellation(t *testing.T) {
	for _, resultErr := range []error{nil, errors.New("fetch failed")} {
		canceled := false
		current := model{
			busy: true, busyCancel: func() { canceled = true },
			selected: make(map[string]bool), remoteSelected: make(map[string]bool),
			progressTitle: "Installing", progressDetail: "Cloning",
		}
		next, quit := current.Update(remoteToggleDone{result: remoteToggleResult{
			Skill: "demo", Selected: make(map[string]bool), RemoteSelected: make(map[string]bool),
		}, err: resultErr})
		finished := next.(model)
		if !canceled || finished.busy || finished.busyCancel != nil || quit != nil {
			t.Fatalf("completion state with error %v retained cancellation or quit unexpectedly", resultErr)
		}
	}
}

func TestDiscoverySkipsNonRegularManifests(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	base := filepath.Join(project, ".agents", "skills")
	writeFile(t, filepath.Join(base, "good", skillManifestName), skillFile("good", "Good.", "good body\n"))
	for _, kind := range []string{"directory", "fifo", "linked-directory", "linked-fifo", "linked-regular"} {
		root := filepath.Join(base, kind)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, skillManifestName)
		switch kind {
		case "directory":
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		case "fifo":
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			target := filepath.Join(root, "target")
			switch kind {
			case "linked-directory":
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "linked-fifo":
				if err := syscall.Mkfifo(target, 0o600); err != nil {
					t.Fatal(err)
				}
			case "linked-regular":
				writeFile(t, target, skillFile("linked", "Linked.", "linked body\n"))
			}
			if err := os.Symlink("target", path); err != nil {
				t.Fatal(err)
			}
		}
	}
	var output bytes.Buffer
	if err := manager.listContext(t.Context(), project, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `name="good"`) || !strings.Contains(output.String(), `name="linked"`) {
		t.Fatalf("valid skills missing from list: %s", &output)
	}
	output.Reset()
	if err := manager.getContext(t.Context(), project, "good", "", &output); err != nil || output.String() != "good body\n" {
		t.Fatalf("valid skill access = %q, %v", &output, err)
	}
}

func TestTUICanUninstallMissingRemoteContent(t *testing.T) {
	for _, global := range []bool{false, true} {
		manager := newTestManager(t)
		manager.global = global
		project := t.TempDir()
		alpha, beta := auditRemoteRef("alpha"), auditRemoteRef("beta")
		for _, ref := range []remoteSkillRef{alpha, beta} {
			if _, err := manager.selectRemote(t.Context(), project, ref, auditProvider(ref.Name), false); err != nil {
				t.Fatal(err)
			}
			auditEvictContent(t, manager.remoteStore, loadRemoteRecord(t, manager.remoteStore, ref))
		}
		current, err := newModel(manager, project)
		if err != nil {
			t.Fatal(err)
		}
		current.width, current.height = 100, 20
		for row, index := range current.localSkillIndices() {
			if current.skills[index].RemoteKey == alpha.key() {
				current.cursor = row
			}
		}
		if index, ok := current.localSkillIndex(current.cursor); !ok || current.skills[index].RemoteKey != alpha.key() || !current.skills[index].ContentMissing {
			t.Fatal("missing-content identity has no uninstallable row")
		}
		if !strings.Contains(current.View(), "[content missing]") {
			t.Fatal("management row does not explain missing content")
		}
		for _, key := range []string{" ", "e", "i", "m", "a"} {
			next, command := current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if command != nil || next.(model).busy || !strings.Contains(next.(model).status, "content missing") {
				t.Fatalf("unavailable action %q was not refused with recovery guidance", key)
			}
		}
		var output bytes.Buffer
		if err := manager.listContext(t.Context(), project, &output); err != nil || strings.Contains(output.String(), `name="alpha"`) {
			t.Fatalf("missing skill leaked to list: %q, %v", &output, err)
		}
		if err := manager.getContext(t.Context(), project, alpha.Name, "", &output); err == nil {
			t.Fatal("missing-content management row was accessible through get")
		}
		if _, err := manager.scriptCommandContext(t.Context(), project, "alpha/script.sh", nil); err == nil {
			t.Fatal("missing-content management row was executable")
		}
		next, uninstall := current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
		if uninstall == nil || !next.(model).busy {
			t.Fatal("missing-content row did not schedule uninstall")
		}
		result := uninstall().(remoteUninstallDone)
		if result.err != nil {
			t.Fatal(result.err)
		}
		next, _ = next.(model).Update(result)
		remaining := next.(model)
		if len(remaining.skills) != 1 || remaining.skills[0].RemoteKey != beta.key() || !remaining.skills[0].ContentMissing {
			t.Fatalf("uninstall dropped another missing identity or retained removed row: %#v", remaining.skills)
		}
		selection, err := loadLock(manager.lockDir(project))
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := selection.remote(alpha.Name); exists {
			t.Fatal("TUI uninstall retained removed selection")
		}
		if ref, exists := selection.remote(beta.Name); !exists || ref != beta {
			t.Fatal("TUI uninstall changed unrelated selection")
		}
	}
}

func TestTUIRegistryReinstallRestoresMissingRemoteRow(t *testing.T) {
	fakeGit(t, map[string]map[string]gitTestFile{"default": {
		"skills/alpha/SKILL.md": {contents: skillFile("alpha", "Remote alpha.", "restored\n"), mode: 0o644},
	}})
	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
	project, ref := t.TempDir(), auditRemoteRef("alpha")
	if _, err := manager.selectRemote(t.Context(), project, ref, auditProvider("alpha"), false); err != nil {
		t.Fatal(err)
	}
	auditEvictContent(t, manager.remoteStore, loadRemoteRecord(t, manager.remoteStore, ref))
	current, err := newModel(manager, project)
	if err != nil {
		t.Fatal(err)
	}
	if current.remoteSelected[ref.key()] {
		t.Fatal("registry treated missing content as an available installation")
	}
	current.tab, current.filterQuery = remoteTab, "alpha"
	current.registrySkills = []registrySearchSkill{{ID: ref.ID, Name: ref.Name, Provider: ref.Provider, Locator: ref.Locator}}
	next, install := toggleSelectedSkill(current)
	if install == nil {
		t.Fatal("registry reinstall was not scheduled")
	}
	result := install().(remoteToggleDone)
	if result.err != nil {
		t.Fatal(result.err)
	}
	next, _ = next.(model).Update(result)
	restored := next.(model)
	if len(restored.allSkills) != 1 || restored.allSkills[0].ContentMissing || !restored.allSkills[0].Editable || !restored.remoteSelected[ref.key()] {
		t.Fatalf("reinstall retained missing-content row: %#v", restored.allSkills)
	}
	var output bytes.Buffer
	if err := manager.getContext(t.Context(), project, ref.Name, "", &output); err != nil || output.String() != "restored\n" {
		t.Fatalf("reinstalled content = %q, %v", &output, err)
	}
}
