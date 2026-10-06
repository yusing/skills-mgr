package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func agentTestCommand(t *testing.T, m *manager, project, input string, args ...string) (string, string, error) {
	t.Helper()
	var out, diagnostics bytes.Buffer
	err := m.agentCommand(t.Context(), project, args, strings.NewReader(input), &out, &diagnostics)
	return out.String(), diagnostics.String(), err
}

func TestAgentCanceledMutationsLeaveStateUntouched(t *testing.T) {
	for _, command := range []string{"set", "edit"} {
		t.Run(command, func(t *testing.T) {
			m := newTestManager(t)
			project := t.TempDir()
			path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
			original := skillFile("alpha", "Alpha.", "Original.\n")
			writeFile(t, path, original)
			agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": false}, nil, nil))
			selection, err := os.ReadFile(filepath.Join(project, lockName))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := []string{"set", "alpha", "true"}
			if command == "edit" {
				args = []string{"edit", "alpha", "--file", "-"}
			}
			var out, diagnostics bytes.Buffer
			err = m.agentCommand(ctx, project, args, strings.NewReader(skillFile("alpha", "Edited.", "Edited.\n")), &out, &diagnostics)
			if !errors.Is(err, context.Canceled) || out.Len() != 0 || diagnostics.Len() != 0 {
				t.Fatalf("canceled mutation = %v, output=%q diagnostics=%q", err, out.String(), diagnostics.String())
			}
			assertFile(t, path, original)
			assertFile(t, filepath.Join(project, lockName), string(selection))
			if _, err := os.Stat(filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md")); !os.IsNotExist(err) {
				t.Fatalf("canceled mutation created placeholder: %v", err)
			}
		})
	}
}

type agentSignaledPipeReader struct {
	*io.PipeReader
	started chan struct{}
}

func (r *agentSignaledPipeReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	return r.PipeReader.Read(p)
}

func TestAgentEditCancellationReleasesBlockedInput(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	original := skillFile("alpha", "Alpha.", "Original.\n")
	writeFile(t, path, original)
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": false}, nil, nil))
	selection, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	input := &agentSignaledPipeReader{reader, make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- m.agentCommand(ctx, project, []string{"edit", "alpha", "--file", "-"}, input, io.Discard, io.Discard)
	}()
	select {
	case <-input.started:
	case err := <-done:
		t.Fatalf("edit ended before reading input: %v", err)
	case <-ctx.Done():
		reader.Close()
		<-done
		t.Fatal("edit did not start reading input before deadline")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled blocked edit did not propagate cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		reader.Close()
		<-done
		t.Fatal("cancel did not release blocked edit input")
	}
	writeDone := make(chan error, 1)
	go func() { _, err := writer.Write([]byte("late input")); writeDone <- err }()
	select {
	case err := <-writeDone:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("reader remains open after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		writer.Close()
		<-writeDone
		t.Fatal("reader was not closed on cancellation")
	}
	assertFile(t, path, original)
	assertFile(t, filepath.Join(project, lockName), string(selection))
}

func TestAgentCheckServedRootBodyHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body        string
		userInvoked, pass bool
	}{
		{"horizontal rule", "---\n# Actual instructions\nUse this skill.\n", false, true},
		{"user invoked placeholder", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			project := t.TempDir()
			content := skillFile("alpha", "Alpha.", tc.body)
			if tc.userInvoked {
				content = strings.Replace(content, "description: Alpha.", "description: Alpha.\ndisable-model-invocation: true", 1)
			}
			writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), content)
			agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, nil))
			out, diagnostics, err := agentTestCommand(t, m, project, "", "check", "alpha")
			if tc.pass {
				if err != nil || out != "PASS alpha\n" || diagnostics != "" {
					t.Fatalf("horizontal rule check = %q %q %v", out, diagnostics, err)
				}
			} else if err == nil || out != "" || !strings.HasPrefix(diagnostics, "FAIL alpha:") {
				t.Fatalf("placeholder check = %q %q %v", out, diagnostics, err)
			}
		})
	}
}

func TestAgentEditRejectsNonhexExpectedDigest(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	original := skillFile("alpha", "Alpha.", "Original.\n")
	writeFile(t, path, original)
	out, diagnostics, err := agentTestCommand(t, m, project, skillFile("alpha", "Edited.", "Edited.\n"), "edit", "alpha", "--file", "-", "--expect-sha256", strings.Repeat("z", 64))
	if err == nil || !strings.Contains(err.Error(), "hexadecimal") || out != "" || diagnostics != "" {
		t.Fatalf("invalid digest = %q %q %v", out, diagnostics, err)
	}
	assertFile(t, path, original)
}

func TestAgentCommandWriterFailuresPropagate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		diagnostic bool
	}{
		{"inspect output", []string{"inspect", "alpha"}, false},
		{"check output", []string{"check", "alpha"}, false},
		{"check diagnostic", []string{"check", "missing"}, true},
		{"set output", []string{"set", "alpha", "true"}, false},
		{"edit output", []string{"edit", "alpha", "--file", "-"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			project := t.TempDir()
			writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", "Body.\n"))
			agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, nil))
			failure := errors.New("test output unavailable")
			broken := writerFunc(func([]byte) (int, error) { return 0, failure })
			var out, diagnostics io.Writer = io.Discard, io.Discard
			if tc.diagnostic {
				diagnostics = broken
			} else {
				out = broken
			}
			err := m.agentCommand(t.Context(), project, tc.args, strings.NewReader(skillFile("alpha", "Edited.", "Edited.\n")), out, diagnostics)
			if !errors.Is(err, failure) {
				t.Fatalf("writer error = %v", err)
			}
		})
	}
}

func TestAgentEditOwnerScopeFromHomeAndProject(t *testing.T) {
	for _, atHome := range []bool{true, false} {
		t.Run(fmt.Sprint(atHome), func(t *testing.T) {
			m := newTestManager(t)
			repo := t.TempDir()
			shared := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
			local := filepath.Join(repo, ".agents", "skills", "alpha", "SKILL.md")
			sharedBody := skillFile("alpha", "Shared.", "Shared body.\n")
			localBody := skillFile("alpha", "Project.", "Project body.\n")
			writeFile(t, shared, sharedBody)
			writeFile(t, local, localBody)
			project, owner := repo, local
			if atHome {
				project, owner = m.paths.placeholderDir, shared
			}
			edited := skillFile("alpha", "Edited.", "Edited body.\n")
			if _, _, err := agentTestCommand(t, m, project, edited, "edit", "alpha", "--file", "-"); err != nil {
				t.Fatal(err)
			}
			assertFile(t, owner, edited)
			if atHome {
				assertFile(t, local, localBody)
			} else {
				assertFile(t, shared, sharedBody)
			}
			report := agentTestInspection(t, m, project, "alpha")
			scope := "project"
			if atHome {
				scope = "shared"
			}
			item := report.Resolved
			if item == nil && len(report.Candidates) == 1 {
				item = &report.Candidates[0]
			}
			if item == nil || item.Path != owner || item.Scope != scope {
				t.Fatalf("scoped owner = %#v", report)
			}
		})
	}
}

func agentTestInspection(t *testing.T, m *manager, project, name string) skillInspection {
	t.Helper()
	out, diagnostics, err := agentTestCommand(t, m, project, "", "inspect", name)
	if err != nil || diagnostics != "" {
		t.Fatalf("inspect: error=%v diagnostics=%q output=%s", err, diagnostics, out)
	}
	var report skillInspection
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range report.Candidates {
		if report.Resolved != nil && candidate.Path == report.Resolved.Path {
			t.Fatalf("resolved owner repeated in candidates: %s", out)
		}
	}
	return report
}

func agentTestSaveLock(t *testing.T, dir string, value lock) {
	t.Helper()
	if err := saveLock(dir, value); err != nil {
		t.Fatal(err)
	}
}

func TestAgentInspectMissingRemoteCache(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	ref := remoteSkillRef{Provider: skillsShProvider, ID: "owner/repo/alpha", Name: "alpha", Locator: "owner/repo/alpha"}
	provider := &staticRemoteProvider{files: []remoteSkillFile{{Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Remote.", "Remote body.\n"))}}}
	record, err := m.remoteStore.ensure(t.Context(), ref, provider)
	if err != nil {
		t.Fatal(err)
	}
	root, err := m.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, map[string]remoteSkillRef{"alpha": ref}))
	if err := os.Rename(root, root+"-removed"); err != nil {
		t.Fatal(err)
	}
	report := agentTestInspection(t, m, project, "alpha")
	if report.Resolved != nil || len(report.Candidates) != 1 || !report.Candidates[0].ContentMissing || !report.Candidates[0].Enabled || report.Candidates[0].BodyHealth != "error" || report.Candidates[0].RemoteKey != ref.key() || !strings.Contains(report.Error, "skills-mgr sync") {
		t.Fatalf("missing-cache inspection = %#v", report)
	}
	claude, _ := agentTestNativeAlternatives(t, m, "alpha")
	report = agentTestInspection(t, m, project, "alpha")
	if report.Resolved == nil || report.Resolved.Path != claude || !report.Fallback {
		t.Fatalf("missing-cache fallback = %#v", report)
	}
}

func TestAgentEditRollbackPreservesSourceAndSelection(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	original := skillFile("alpha", "Alpha.", "Original.\n")
	writeFile(t, path, original)
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, map[string]string{"beta": "lang go"}, nil))
	selection, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = agentTestCommand(t, m, project, skillFile("beta", "Renamed.", "New instructions.\n"), "edit", "alpha", "--file", "-")
	if err == nil || !strings.Contains(err.Error(), "cannot merge enabled values") {
		t.Fatalf("rename conflict = %v", err)
	}
	assertFile(t, path, original)
	assertFile(t, filepath.Join(project, lockName), string(selection))
}

func TestAgentCheckManifestAliases(t *testing.T) {
	for _, target := range []string{"alpha", "alpha/SKILL.md", "alpha/./SKILL.md", "alpha/references/../SKILL.md"} {
		for _, userInvoked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/user-invoked=%v", target, userInvoked), func(t *testing.T) {
				m := newTestManager(t)
				project := t.TempDir()
				root := filepath.Join(m.paths.managedSkills, "alpha")
				body := skillFile("alpha", "Alpha.", "")
				if userInvoked {
					body = strings.Replace(body, "description: Alpha.", "description: Alpha.\ndisable-model-invocation: true", 1)
				}
				writeFile(t, filepath.Join(root, "SKILL.md"), body)
				writeFile(t, filepath.Join(root, "references", "guide.md"), "Guide.\n")
				agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, nil))
				out, diagnostic, err := agentTestCommand(t, m, project, "", "check", target)
				if err == nil || strings.Contains(out, "PASS "+target+"\n") || !strings.Contains(diagnostic, "FAIL "+target+":") {
					t.Fatalf("empty alias check = %q %q %v", out, diagnostic, err)
				}
				body += "Actual instructions.\n"
				writeFile(t, filepath.Join(root, "SKILL.md"), body)
				writeFile(t, filepath.Join(root, "references", "guide.md"), "")
				out, diagnostic, err = agentTestCommand(t, m, project, "", "check", target)
				if err == nil || !strings.Contains(out, "PASS "+target+"\n") || !strings.Contains(diagnostic, "FAIL alpha/references/guide.md:") {
					t.Fatalf("alias reference expansion = %q %q %v", out, diagnostic, err)
				}
			})
		}
	}
}

func TestAgentInspectMissingCacheSelectionErrorsDoNotBlockNative(t *testing.T) {
	for _, expression := range []string{"if", "exit 2", "false"} {
		t.Run(expression, func(t *testing.T) {
			m := newTestManager(t)
			project := t.TempDir()
			ref := remoteSkillRef{Provider: skillsShProvider, ID: "owner/repo/alpha", Name: "alpha", Locator: "owner/repo/alpha"}
			provider := &staticRemoteProvider{files: []remoteSkillFile{{Path: "SKILL.md", Contents: []byte(skillFile("alpha", "Remote.", "Remote body.\n"))}}}
			record, err := m.remoteStore.ensure(t.Context(), ref, provider)
			if err != nil {
				t.Fatal(err)
			}
			root, err := m.remoteStore.contentRoot(record)
			if err != nil {
				t.Fatal(err)
			}
			agentTestSaveLock(t, project, testLock(nil, map[string]string{"alpha": expression}, map[string]remoteSkillRef{"alpha": ref}))
			if err := os.Rename(root, root+"-removed"); err != nil {
				t.Fatal(err)
			}
			claude, grok := agentTestNativeAlternatives(t, m, "alpha")
			for _, native := range []string{claude, grok} {
				if native == grok {
					writeJSONFile(t, m.paths.claudeSettings, claudeSettingsFile{EnabledPlugins: map[string]bool{"sample@market": false}})
				}
				var served bytes.Buffer
				if err := m.getContext(t.Context(), project, "alpha", "", &served); err != nil {
					t.Fatal(err)
				}
				report := agentTestInspection(t, m, project, "alpha")
				if report.Resolved == nil || report.Resolved.Path != native || !report.Fallback || report.Error != "" || !report.Candidates[0].ContentMissing || report.Candidates[0].Error == "" {
					t.Fatalf("diagnostic blocked native resolution = %#v", report)
				}
			}
		})
	}
}

func TestAgentInspectSelectionLayers(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	body := skillFile("alpha", "Alpha.", "Actual instructions.\n")
	writeFile(t, path, body)
	writeFile(t, filepath.Join(filepath.Dir(path), "references", "guide.md"), "Guide.\n")
	for _, tc := range []struct {
		name            string
		global, project lock
		layer           string
		enabled         bool
		expression      string
	}{
		{"default", newLock(), newLock(), "default", false, ""},
		{"global boolean", testLock(map[string]bool{"alpha": true}, nil, nil), newLock(), "global", true, ""},
		{"project overrides", testLock(map[string]bool{"alpha": true}, nil, nil), testLock(map[string]bool{"alpha": false}, nil, nil), "project", false, ""},
		{"project expression", testLock(map[string]bool{"alpha": false}, nil, nil), testLock(nil, map[string]string{"alpha": "test -f marker"}, nil), "project", true, "test -f marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, filepath.Join(project, "marker"), "present")
			agentTestSaveLock(t, m.paths.globalLockDir, tc.global)
			agentTestSaveLock(t, project, tc.project)
			report := agentTestInspection(t, m, project, "alpha")
			var item inspectedSkill
			if tc.enabled {
				if report.Resolved == nil || len(report.Candidates) != 0 {
					t.Fatalf("enabled inspection = %#v", report)
				}
				item = *report.Resolved
			} else {
				if report.Resolved != nil || len(report.Candidates) != 1 {
					t.Fatalf("disabled inspection = %#v", report)
				}
				item = report.Candidates[0]
			}
			if report.Name != "alpha" || report.Project != project || item.Path != path || !item.Editable || item.Source == "" || item.Enabled != tc.enabled || item.Selection.Layer != tc.layer || item.BodyHealth != "ok" || item.Error != "" || item.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(body))) || !slices.Equal(item.References, []string{"references/guide.md"}) {
				t.Fatalf("inspection = %#v", report)
			}
			if item.Selection.Value == nil {
				t.Fatal("missing effective selection value")
			}
			if tc.expression != "" {
				if item.Selection.Value.Expression != tc.expression {
					t.Fatalf("selection = %#v", item.Selection)
				}
			} else if item.Selection.Value.Boolean == nil || *item.Selection.Value.Boolean != tc.enabled {
				t.Fatalf("selection = %#v", item.Selection)
			}
			if (report.Resolved != nil) != tc.enabled {
				t.Fatalf("resolved = %#v", report.Resolved)
			}
		})
	}
	if _, _, err := agentTestCommand(t, m, project, "", "inspect", "absent"); err == nil {
		t.Fatal("absent inspection succeeded")
	}
}

func agentTestNativeAlternatives(t *testing.T, m *manager, name string) (string, string) {
	t.Helper()
	pluginRoot := filepath.Join(m.paths.claudePlugins, "cache", "market", "sample", "1.0.0")
	claudePath := filepath.Join(pluginRoot, "skills", name, "SKILL.md")
	writeFile(t, claudePath, skillFile(name, "Claude.", "Claude instructions.\n"))
	writeJSONFile(t, m.paths.claudeSettings, claudeSettingsFile{EnabledPlugins: map[string]bool{"sample@market": true}})
	writeJSONFile(t, filepath.Join(m.paths.claudePlugins, "installed_plugins.json"), claudeInstalledPluginsFile{Plugins: map[string][]struct {
		InstallPath string `json:"installPath"`
	}{"sample@market": {{InstallPath: pluginRoot}}}})
	grokPath := filepath.Join(t.TempDir(), name, "SKILL.md")
	writeFile(t, grokPath, skillFile(name, "Grok.", "Grok instructions.\n"))
	data, err := json.Marshal(map[string]any{"skills": []any{map[string]any{"name": name, "description": "Grok.", "source": map[string]string{"type": "bundled", "path": grokPath}, "userInvocable": true}}})
	if err != nil {
		t.Fatal(err)
	}
	m.paths.grokCommand = writeGrokInspectCommand(t, string(data))
	return claudePath, grokPath
}

func TestAgentInspectFilesystemFirstAndNativeFallback(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	owner := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	writeFile(t, owner, skillFile("alpha", "Owner.", "Owner instructions.\n"))
	claude, grok := agentTestNativeAlternatives(t, m, "alpha")
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, nil))
	report := agentTestInspection(t, m, project, "alpha")
	if len(report.Candidates) != 2 || report.Resolved == nil || report.Resolved.Path != owner || report.Fallback {
		t.Fatalf("filesystem resolution = %#v", report)
	}
	if report.Candidates[0].Path != claude || report.Candidates[1].Path != grok || report.Candidates[0].Editable || report.Candidates[1].Editable {
		t.Fatalf("native candidates = %#v", report.Candidates)
	}
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": false}, nil, nil))
	report = agentTestInspection(t, m, project, "alpha")
	if report.Resolved == nil || report.Resolved.Path != claude || !report.Fallback || len(report.Candidates) != 2 || report.Candidates[0].Path != owner || report.Candidates[1].Path != grok {
		t.Fatalf("Claude fallback = %#v", report)
	}
	writeJSONFile(t, m.paths.claudeSettings, claudeSettingsFile{EnabledPlugins: map[string]bool{"sample@market": false}})
	report = agentTestInspection(t, m, project, "alpha")
	if report.Resolved == nil || report.Resolved.Path != grok || !report.Fallback || len(report.Candidates) != 2 || report.Candidates[0].Path != owner || report.Candidates[1].Path != claude {
		t.Fatalf("Grok fallback = %#v", report)
	}
}

func TestAgentCheckResourcesContinuesWithoutExecuting(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(m.paths.managedSkills, "alpha")
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("alpha", "Alpha.", "Instructions.\n"))
	writeFile(t, filepath.Join(root, "guide.md"), "Guide.\n")
	writeFile(t, filepath.Join(root, "references", "nested", "ok.md"), "Nested guide.\n")
	writeFile(t, filepath.Join(root, "references", "empty.md"), " \n")
	writeFile(t, filepath.Join(root, "references", "placeholder.md"), skillFile("stub", "Stub.", ""))
	writeFile(t, filepath.Join(root, "dependencies", "explicit.txt"), "Dependency data.\n")
	marker := filepath.Join(project, "executed")
	writeExecutable(t, filepath.Join(root, "scripts", "record.sh"), "#!/bin/sh\ntouch '"+marker+"'\n")
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, nil))
	out, diagnostics, err := agentTestCommand(t, m, project, "", "check", "missing", "alpha", "alpha/dependencies/explicit.txt", "alpha/scripts/record.sh")
	if err == nil {
		t.Fatal("failed resources yielded success")
	}
	for _, target := range []string{"alpha", "alpha/guide.md", "alpha/references/nested/ok.md", "alpha/dependencies/explicit.txt", "alpha/scripts/record.sh"} {
		if !slices.Contains(strings.Split(strings.TrimSpace(out), "\n"), "PASS "+target) {
			t.Errorf("missing PASS %s in %q", target, out)
		}
	}
	for _, target := range []string{"missing", "alpha/references/empty.md", "alpha/references/placeholder.md"} {
		if !strings.Contains(diagnostics, "FAIL "+target+":") {
			t.Errorf("missing failure %s in %q", target, diagnostics)
		}
	}
	if strings.Contains(out, "FAIL") || strings.Contains(diagnostics, "PASS") {
		t.Fatalf("mixed output streams: %q / %q", out, diagnostics)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("check executed script: %v", err)
	}
}

func TestAgentCheckRejectsRootWithNoBody(t *testing.T) {
	for _, body := range []string{"", " \n"} {
		t.Run(fmt.Sprintf("body_%q", body), func(t *testing.T) {
			m := newTestManager(t)
			project := t.TempDir()
			writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", body))
			agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, nil))
			out, diagnostics, err := agentTestCommand(t, m, project, "", "check", "alpha")
			if err == nil || out != "" || !strings.Contains(diagnostics, "FAIL alpha:") {
				t.Fatalf("check = %q %q %v", out, diagnostics, err)
			}
		})
	}
}

func TestAgentSetSelectionAndPlaceholders(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	content := skillFile("alpha", "Alpha.", "Body.\n")
	writeFile(t, path, content)
	for _, value := range []string{"true", "false", "test -f marker", "  test -f marker  ", "inherit"} {
		if _, _, err := agentTestCommand(t, m, project, "", "set", "alpha", value); err != nil {
			t.Fatalf("set %q: %v", value, err)
		}
		selection, err := loadLock(project)
		if err != nil {
			t.Fatal(err)
		}
		current, exists := selection.enabled("alpha")
		switch value {
		case "true", "false":
			if !hasBooleanSelection(selection, "alpha", value == "true") {
				t.Fatalf("selection = %#v", current)
			}
		case "inherit":
			if exists {
				t.Fatal("inherit retained override")
			}
		default:
			if !exists || current.Expression != value {
				t.Fatalf("condition = %#v", current)
			}
		}
		placeholder := filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md")
		if value != "false" && value != "inherit" {
			assertFile(t, placeholder, wantPlaceholder("alpha", "Alpha."))
		} else if _, err := os.Stat(placeholder); !os.IsNotExist(err) {
			t.Fatalf("disabled placeholder: %v", err)
		}
		assertFile(t, path, content)
	}
	if _, _, err := agentTestCommand(t, m, project, "", "set", "alpha", "true"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", " ", "if then"} {
		before, err := os.ReadFile(filepath.Join(project, lockName))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := agentTestCommand(t, m, project, "", "set", "alpha", invalid); err == nil {
			t.Fatalf("invalid condition %q accepted", invalid)
		}
		assertFile(t, filepath.Join(project, lockName), string(before))
	}
}

func TestAgentSetGlobalAndHome(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			m := newTestManager(t)
			writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", "Body.\n"))
			project := m.paths.placeholderDir
			args := []string{"set", "alpha", "true"}
			if explicit {
				project = t.TempDir()
				args = []string{"set", "-g", "alpha", "true"}
			}
			if _, _, err := agentTestCommand(t, m, project, "", args...); err != nil {
				t.Fatal(err)
			}
			selection, err := loadLock(m.paths.globalLockDir)
			if err != nil || !hasBooleanSelection(selection, "alpha", true) {
				t.Fatalf("global selection=%#v error=%v", selection, err)
			}
			assertFile(t, filepath.Join(m.paths.placeholderDir, ".agents", "skills", "alpha", "SKILL.md"), wantPlaceholder("alpha", "Alpha."))
			if _, err := os.Stat(filepath.Join(project, lockName)); !os.IsNotExist(err) {
				t.Fatalf("project lock written: %v", err)
			}
		})
	}
}

func TestAgentSetInheritRestoresGlobalSelection(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", "Body.\n"))
	agentTestSaveLock(t, m.paths.globalLockDir, testLock(map[string]bool{"alpha": true}, nil, nil))
	for _, value := range []string{"false", "inherit"} {
		if _, _, err := agentTestCommand(t, m, project, "", "set", "alpha", value); err != nil {
			t.Fatal(err)
		}
	}
	report := agentTestInspection(t, m, project, "alpha")
	if report.Resolved == nil || report.Resolved.Selection.Layer != "global" || !report.Resolved.Enabled {
		t.Fatalf("inherited selection = %#v", report)
	}
	for _, harness := range []string{".agents", ".claude"} {
		assertFile(t, filepath.Join(project, harness, "skills", "alpha", "SKILL.md"), wantPlaceholder("alpha", "Alpha."))
	}
}

func TestAgentCommandRejectsInvalidArguments(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	for _, args := range [][]string{
		{"inspect"}, {"inspect", "alpha", "extra"}, {"check"},
		{"set", "alpha"}, {"set", "-g"},
		{"edit", "alpha"}, {"edit", "alpha", "--wrong", "-"},
		{"edit", "alpha", "--file", "-", "--wrong", "digest"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, diagnostics, err := agentTestCommand(t, m, project, "", args...)
			if err == nil || out != "" || diagnostics != "" {
				t.Fatalf("invalid arguments result = %q %q %v", out, diagnostics, err)
			}
		})
	}
}

func TestAgentNativeOnlyCannotBeManaged(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	claude, grok := agentTestNativeAlternatives(t, m, "alpha")
	for _, args := range [][]string{{"set", "alpha", "true"}, {"edit", "alpha", "--file", "-"}} {
		if _, _, err := agentTestCommand(t, m, project, skillFile("alpha", "Changed.", "Changed.\n"), args...); err == nil {
			t.Fatalf("native management accepted %v", args)
		}
	}
	assertFile(t, claude, skillFile("alpha", "Claude.", "Claude instructions.\n"))
	assertFile(t, grok, skillFile("alpha", "Grok.", "Grok instructions.\n"))
	if _, err := os.Stat(filepath.Join(project, lockName)); !os.IsNotExist(err) {
		t.Fatalf("native management created selection: %v", err)
	}
}

func TestAgentEditLocalValidationDigestAndRename(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md")
	original := skillFile("alpha", "Alpha.", "Original.\n")
	writeFile(t, path, original)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agentTestCommand(t, m, project, "", "set", "alpha", "true"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "body without metadata", skillFile("alpha", "Alpha.", ""), "---\nname: [\n---\nbody\n"} {
		if _, _, err := agentTestCommand(t, m, project, invalid, "edit", "alpha", "--file", "-"); err == nil {
			t.Fatalf("invalid edit accepted %q", invalid)
		}
		assertFile(t, path, original)
	}
	valid := skillFile("alpha", "Edited.", "Edited instructions.\n")
	if _, _, err := agentTestCommand(t, m, project, valid, "edit", "alpha", "--file", "-", "--expect-sha256", strings.Repeat("0", 64)); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	assertFile(t, path, original)
	inputPath := filepath.Join(t.TempDir(), "replacement.md")
	writeFile(t, inputPath, valid)
	if _, _, err := agentTestCommand(t, m, project, "ignored", "edit", "alpha", "--file", inputPath, "--expect-sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(original)))); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, valid)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode())
	}
	writeFile(t, filepath.Join(m.paths.managedSkills, "taken", "SKILL.md"), skillFile("taken", "Taken.", "Body.\n"))
	if _, _, err := agentTestCommand(t, m, project, skillFile("taken", "Collision.", "Body.\n"), "edit", "alpha", "--file", "-"); err == nil {
		t.Fatal("name collision accepted")
	}
	assertFile(t, path, valid)
	renamed := skillFile("beta", "Renamed.", "New body.\n")
	if _, _, err := agentTestCommand(t, m, project, renamed, "edit", "alpha", "--file", "-"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, renamed)
	report := agentTestInspection(t, m, project, "beta")
	if report.Resolved == nil || report.Resolved.Path != path {
		t.Fatalf("renamed source = %#v", report)
	}
	selection, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := selection.enabled("alpha"); exists || !hasBooleanSelection(selection, "beta", true) {
		t.Fatalf("renamed selection = %#v", selection)
	}
	assertFile(t, filepath.Join(project, ".agents", "skills", "beta", "SKILL.md"), wantPlaceholder("beta", "Renamed."))
	if _, err := os.Stat(filepath.Join(project, ".agents", "skills", "alpha")); !os.IsNotExist(err) {
		t.Fatalf("old placeholder retained: %v", err)
	}
}

func TestAgentEditRemotePatchAndStaleHealth(t *testing.T) {
	m := newTestManager(t)
	project := t.TempDir()
	ref := remoteSkillRef{Provider: skillsShProvider, ID: "owner/repo/alpha", Name: "alpha", Locator: "owner/repo/alpha"}
	original := skillFile("alpha", "Remote.", "Remote instructions.\n")
	provider := &staticRemoteProvider{files: []remoteSkillFile{{Path: "SKILL.md", Contents: []byte(original)}}}
	record, err := m.remoteStore.ensure(t.Context(), ref, provider)
	if err != nil {
		t.Fatal(err)
	}
	root, err := m.remoteStore.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(root, "SKILL.md")
	agentTestSaveLock(t, project, testLock(map[string]bool{"alpha": true}, nil, map[string]remoteSkillRef{"alpha": ref}))
	for _, invalid := range []string{skillFile("renamed", "Remote.", "Body.\n"), strings.Replace(original, "description: Remote.", "description: Remote.\ndisable-model-invocation: true", 1)} {
		if _, _, err := agentTestCommand(t, m, project, invalid, "edit", "alpha", "--file", "-"); err == nil {
			t.Fatal("remote identity/invocation edit accepted")
		}
		if _, err := os.Stat(m.remoteStore.patchPath(ref)); !os.IsNotExist(err) {
			t.Fatalf("rejected edit wrote patch: %v", err)
		}
	}
	edited := skillFile("alpha", "Edited remote.", "Locally edited instructions.\n")
	if _, _, err := agentTestCommand(t, m, project, edited, "edit", "alpha", "--file", "-", "--expect-sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(original)))); err != nil {
		t.Fatal(err)
	}
	assertFile(t, basePath, original)
	report := agentTestInspection(t, m, project, "alpha")
	if report.Resolved == nil || report.Resolved.RemoteKey != ref.key() || report.Resolved.PatchPath != m.remoteStore.patchPath(ref) || report.Resolved.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(edited))) || report.Resolved.BodyHealth != "ok" {
		t.Fatalf("patched inspection = %#v", report)
	}
	var served bytes.Buffer
	if err := m.getContext(t.Context(), project, "alpha", "", &served); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(served.String(), "Locally edited instructions.\n") {
		t.Fatalf("served output=%q", served.String())
	}
	patchBefore, err := os.ReadFile(m.remoteStore.patchPath(ref))
	if err != nil {
		t.Fatal(err)
	}
	provider.files[0].Contents = []byte(skillFile("alpha", "Upstream.", "Changed upstream instructions.\n"))
	if err := m.remoteStore.refresh(t.Context(), ageRemoteRecord(t, m.remoteStore, ref), provider); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agentTestCommand(t, m, project, edited, "edit", "alpha", "--file", "-"); err == nil {
		t.Fatal("stale patch edit succeeded")
	}
	assertFile(t, m.remoteStore.patchPath(ref), string(patchBefore))
	report = agentTestInspection(t, m, project, "alpha")
	if report.Resolved == nil || len(report.Candidates) != 0 || report.Resolved.BodyHealth == "ok" || report.Resolved.Error == "" {
		t.Fatalf("stale health = %#v", report)
	}
	out, diagnostics, err := agentTestCommand(t, m, project, "", "check", "alpha")
	if err == nil || out != "" || !strings.Contains(diagnostics, "FAIL alpha:") {
		t.Fatalf("stale check=%q %q %v", out, diagnostics, err)
	}
}

func TestAgentRunDispatch(t *testing.T) {
	taskHome := t.TempDir()
	t.Setenv("HOME", taskHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(taskHome, "cache"))
	t.Setenv("CODEX_HOME", filepath.Join(taskHome, ".codex"))
	project := t.TempDir()
	t.Chdir(project)
	path := filepath.Join(taskHome, ".skills-mgr", "skills", "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Alpha.", "Original.\n"))
	draft := filepath.Join(t.TempDir(), "draft.md")
	writeFile(t, draft, skillFile("alpha", "Edited.", "Edited.\n"))
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	originalOut, originalErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() { os.Stdout, os.Stderr = originalOut, originalErr; stdout.Close(); stderr.Close() })
	for _, args := range [][]string{{"set", "alpha", "true"}, {"edit", "alpha", "--file", draft}, {"inspect", "alpha"}, {"check", "alpha"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}
	assertFile(t, path, skillFile("alpha", "Edited.", "Edited.\n"))
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "PASS alpha\n") || !strings.Contains(string(data), `"name": "alpha"`) {
		t.Fatalf("dispatch output=%q", data)
	}
	assertFile(t, stderr.Name(), "")
}
