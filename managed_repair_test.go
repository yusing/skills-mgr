package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepairManagedPlaceholders(t *testing.T) {
	for _, scope := range []string{"home", "manager-home", "global", "project"} {
		for _, state := range []string{"enabled", "conditional", "disabled", "unselected", "inherited"} {
			t.Run(scope+"/"+state, func(t *testing.T) {
				m := newTestManager(t)
				project := t.TempDir()
				base, selectionDir := project, project
				switch scope {
				case "home":
					project = m.paths.placeholderDir
				case "manager-home":
					project = m.paths.globalLockDir
				case "global":
					m.global = true
				}
				if scope != "project" {
					base, selectionDir = m.paths.placeholderDir, m.paths.globalLockDir
				}
				content := "---\nname: orchestrated-workflow\ndescription: Workflow.\ndisable-model-invocation: true\n---\nbody\n"
				source := filepath.Join(m.paths.managedSkills, "orchestrated-workflow", "SKILL.md")
				writeFile(t, source, content)
				value := newLock()
				switch state {
				case "enabled":
					value.setEnabled("orchestrated-workflow", enabledValue{Boolean: new(true)})
				case "conditional":
					value.setEnabled("orchestrated-workflow", enabledValue{Expression: "false"})
				case "disabled":
					value.setEnabled("orchestrated-workflow", enabledValue{Boolean: new(false)})
				}
				if err := saveLock(selectionDir, value); err != nil {
					t.Fatal(err)
				}
				if state == "inherited" && scope == "project" {
					if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"orchestrated-workflow": true}, nil, nil)); err != nil {
						t.Fatal(err)
					}
					if _, err := m.changeRemotePlaceholdersAcross([]placeholderChange{{base: m.paths.placeholderDir, name: "orchestrated-workflow", enabled: true}}, "name: orchestrated-workflow\ndescription: Workflow.\n"); err != nil {
						t.Fatal(err)
					}
				}
				// Seed stale stubs for cleanup cases; enabled cases start with missing stubs.
				want := state == "enabled" || state == "conditional"
				if !want {
					if _, err := m.changeRemotePlaceholdersAcross([]placeholderChange{{base: base, name: "orchestrated-workflow", enabled: true}}, "name: orchestrated-workflow\ndescription: Workflow.\n"); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if err := m.repairManagedPlaceholders(t.Context(), project, testLoggerSink()); err != nil {
						t.Fatal(err)
					}
					for _, root := range []string{".agents", ".claude"} {
						path := filepath.Join(base, root, "skills", "orchestrated-workflow", "SKILL.md")
						if want {
							assertFile(t, path, wantPlaceholder("orchestrated-workflow", "Workflow."))
						} else if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatalf("unselected placeholder remains: %s: %v", path, err)
						}
						if state == "inherited" && scope == "project" {
							assertFile(t, filepath.Join(m.paths.placeholderDir, root, "skills", "orchestrated-workflow", "SKILL.md"), wantPlaceholder("orchestrated-workflow", "Workflow."))
						}
					}

				}
				assertFile(t, source, content)
			})
		}
	}
}

func TestRepairManagedRollsBackOnFailure(t *testing.T) {
	m := newTestManager(t)
	project := m.paths.placeholderDir
	for _, name := range []string{"alpha", "beta"} {
		writeFile(t, filepath.Join(m.paths.managedSkills, name, "SKILL.md"), skillFile(name, "Managed.", "body"))
	}
	if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": true, "beta": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(project, ".claude", "skills", "beta", "SKILL.md")
	writeFile(t, collision, "user content")
	if err := m.repairManagedPlaceholders(t.Context(), project, testLoggerSink()); err == nil {
		t.Fatal("repair succeeded")
	}
	for _, root := range []string{".agents", ".claude"} {
		path := filepath.Join(project, root, "skills", "alpha")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("rollback left %s: %v", path, err)
		}
	}
	assertFile(t, collision, "user content")
	assertLock(t, m.paths.globalLockDir, map[string]bool{"alpha": true, "beta": true})
}

func TestRepairManagedDoesNotStubUnmanagedSkill(t *testing.T) {
	m := newTestManager(t)
	source := filepath.Join(m.paths.userSkills, "alpha", "SKILL.md")
	content := skillFile("alpha", "User skill.", "body")
	writeFile(t, source, content)
	if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := m.repairManagedPlaceholders(t.Context(), m.paths.placeholderDir, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	assertFile(t, source, content)
	if _, err := os.Stat(filepath.Join(m.paths.claudeSkills, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("unmanaged stub: %v", err)
	}
}

func TestRepairManagedGlobalIgnoresProjectShadow(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			m := newTestManager(t)
			m.global = true
			project := t.TempDir()
			writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Managed.", "body"))
			projectPath := filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md")
			projectContent := skillFile("alpha", "Project.", "body")
			writeFile(t, projectPath, projectContent)
			if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": enabled}, nil, nil)); err != nil {
				t.Fatal(err)
			}
			if !enabled {
				if err := m.setRemotePlaceholders(project, "alpha", "name: alpha\ndescription: Managed.\n", true); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.repairManagedPlaceholders(t.Context(), project, testLoggerSink()); err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{".agents", ".claude"} {
				path := filepath.Join(m.paths.placeholderDir, root, "skills", "alpha", "SKILL.md")
				if enabled {
					assertFile(t, path, wantPlaceholder("alpha", "Managed."))
				} else if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("disabled stub remains: %v", err)
				}
			}
			assertFile(t, projectPath, projectContent)
		})
	}
}

func TestRefreshRunnerRepairsManagedPlaceholders(t *testing.T) {
	m := newTestManager(t)
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Managed.", "body"))
	if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := runRefreshRunner(t.Context(), m, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{".agents", ".claude"} {
		assertFile(t, filepath.Join(m.paths.placeholderDir, root, "skills", "alpha", "SKILL.md"), wantPlaceholder("alpha", "Managed."))
	}
	if _, err := os.Stat(m.paths.refreshSuccess); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRunnerRepairFailureRetried(t *testing.T) {
	m := newTestManager(t)
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Managed.", "body"))
	if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(m.paths.claudeSkills, "alpha", "SKILL.md")
	writeFile(t, collision, "user content")
	logger, logs := testLogger()
	if err := runRefreshRunner(t.Context(), m, logger); err != nil {
		t.Fatal(err)
	}
	assertFile(t, collision, "user content")
	if _, err := os.Stat(m.paths.refreshSuccess); !os.IsNotExist(err) {
		t.Fatalf("failed repair marked successful: %v", err)
	}
	if !strings.Contains(logs.String(), "repair global managed placeholders") {
		t.Fatalf("missing repair error: %s", logs.String())
	}
}

func TestRefreshRunnerRepairsDespiteRemoteFailure(t *testing.T) {
	m := newTestManager(t)
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Managed.", "body"))
	if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	stubSkillsShRegistry(t, m, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if err := runRefreshRunner(t.Context(), m, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(m.paths.userSkills, "alpha", "SKILL.md"), wantPlaceholder("alpha", "Managed."))
	if _, err := os.Stat(m.paths.refreshSuccess); !os.IsNotExist(err) {
		t.Fatalf("failed cycle marked successful: %v", err)
	}
}

func TestManagedRepairWaitsForForegroundTransaction(t *testing.T) {
	m := newTestManager(t)
	writeFile(t, filepath.Join(m.paths.managedSkills, "alpha", "SKILL.md"), skillFile("alpha", "Managed.", "body"))
	if err := saveLock(m.paths.globalLockDir, testLock(map[string]bool{"alpha": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	guard, err := m.lockManagedMutation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A foreground mutation may have saved its selection but not applied stubs.
	// Repair must not observe that intermediate state or alter its filesystem plan.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = m.repairManagedPlaceholders(ctx, m.paths.placeholderDir, testLoggerSink())
	closeExclusiveLock(guard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repair bypassed transaction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.paths.userSkills, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("repair changed in-flight placeholders: %v", err)
	}
	if err := m.repairManagedPlaceholders(t.Context(), m.paths.placeholderDir, testLoggerSink()); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(m.paths.userSkills, "alpha", "SKILL.md"), wantPlaceholder("alpha", "Managed."))
}

func TestManagedRepairLogsMissingProject(t *testing.T) {
	m := newTestManager(t)
	logger, logs := testLogger()
	if err := m.repairManagedPlaceholders(t.Context(), filepath.Join(t.TempDir(), "missing"), logger); err == nil {
		t.Fatal("repair succeeded with missing project")
	}
	if !strings.Contains(logs.String(), "resolve managed placeholder repair root") {
		t.Fatalf("missing error log: %s", logs.String())
	}
}
