package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInfoCommand(t *testing.T) {
	taskHome := t.TempDir()
	t.Setenv("HOME", taskHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(taskHome, "cache"))
	t.Setenv("CODEX_HOME", filepath.Join(taskHome, "custom-codex"))
	project := t.TempDir()
	t.Chdir(project)
	sharedSkills := filepath.Join(project, ".agents", "skills")
	if err := os.MkdirAll(sharedSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(taskHome, lockName)
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalStart := startBackgroundRefresh
	startBackgroundRefresh = func(*manager, *os.File) error {
		t.Fatal("info started a background refresh")
		return nil
	}
	t.Cleanup(func() { startBackgroundRefresh = originalStart })
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
	if err := run([]string{"info"}); err != nil {
		t.Fatal(err)
	}
	os.Stdout = originalStdout
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Metadata:\n", "Skills:\n", "Remote skills:\n",
		"Project selection: " + filepath.Join(project, lockName) + " [MISSING]\n",
		filepath.Join(taskHome, ".skills-mgr", lockName),
		filepath.Join(taskHome, "cache", "skills-mgr", "skills-sh.json"),
		filepath.Join(taskHome, "cache", "skills-mgr", "skillsmp.json"),
		"Project shared: " + sharedSkills + "\n",
		filepath.Join(taskHome, ".skills-mgr", "skills"),
		filepath.Join(taskHome, "custom-codex", "skills"),
		"Store: " + filepath.Join(taskHome, "cache", "skills-mgr", "remote-skills") + " [MISSING]\n",
		filepath.Join(taskHome, ".skills-mgr", "skills", remoteSkillPatchDir),
	} {
		if runtime.GOOS != "windows" {
			want = strings.ReplaceAll(want, taskHome+"/", "~/")
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("info output missing %q", want)
		}
	}
	assertFile(t, legacy, "{}")
	if _, err := os.Stat(filepath.Join(taskHome, ".skills-mgr")); !os.IsNotExist(err) {
		t.Fatalf("info created manager state: %v", err)
	}
	if err := run([]string{"info", "extra"}); err == nil || err.Error() != "usage: skills-mgr info" {
		t.Fatalf("info with operand error = %v", err)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdout
	if err := run([]string{"info"}); err == nil {
		t.Fatal("info succeeded with closed stdout")
	}
}
