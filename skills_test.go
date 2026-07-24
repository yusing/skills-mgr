package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestToggleUpdatesOnlyProjectLock(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, skill, skillFile("alpha", "Alpha description.", "body"))

	enabled, err := manager.toggle(project, "alpha")
	if err != nil || !enabled {
		t.Fatalf("enable = %v, %v", enabled, err)
	}
	assertLock(t, project, map[string]bool{"alpha": true})
	assertFile(t, skill, skillFile("alpha", "Alpha description.", "body"))
	if _, err := os.Lstat(filepath.Join(project, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("toggle created an installation: %v", err)
	}

	enabled, err = manager.toggle(project, "alpha")
	if err != nil || enabled {
		t.Fatalf("disable = %v, %v", enabled, err)
	}
	assertLock(t, project, map[string]bool{"alpha": false})
	assertFile(t, skill, skillFile("alpha", "Alpha description.", "body"))
}

func TestSelectionInheritsGlobalStateAndAppliesProjectOverrides(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	if err := saveLock(manager.paths.globalLockDir, lock{Skills: map[string]bool{
		"global-enabled":  true,
		"global-disabled": false,
		"overridden":      true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, lock{Skills: map[string]bool{
		"project-enabled": true,
		"overridden":      false,
	}}); err != nil {
		t.Fatal(err)
	}

	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"global-enabled":  true,
		"global-disabled": false,
		"project-enabled": true,
		"overridden":      false,
	}
	if len(selected) != len(want) {
		t.Fatalf("selection = %#v, want %#v", selected, want)
	}
	for skill, enabled := range want {
		if selected[skill] != enabled {
			t.Fatalf("selection = %#v, want %#v", selected, want)
		}
	}
}

func TestToggleCreatesProjectOverrideForInheritedState(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}

	enabled, err := manager.toggle(project, "alpha")
	if err != nil || enabled {
		t.Fatalf("toggle inherited skill = %v, %v; want disabled", enabled, err)
	}
	assertLock(t, project, map[string]bool{"alpha": false})
	assertLock(t, manager.paths.globalLockDir, map[string]bool{"alpha": true})
}

func TestGlobalToggleUpdatesOnlyGlobalLock(t *testing.T) {
	manager := newTestManager(t)
	manager.global = true
	project := t.TempDir()
	if err := saveLock(project, lock{Skills: map[string]bool{"alpha": false}}); err != nil {
		t.Fatal(err)
	}

	enabled, err := manager.toggle(project, "alpha")
	if err != nil || !enabled {
		t.Fatalf("global toggle = %v, %v; want enabled", enabled, err)
	}
	assertLock(t, manager.paths.globalLockDir, map[string]bool{"alpha": true})
	assertLock(t, project, map[string]bool{"alpha": false})

	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	if !selected["alpha"] {
		t.Fatalf("global selection = %#v, want alpha enabled", selected)
	}
}

func TestConcurrentGlobalTogglesPreserveUpdates(t *testing.T) {
	manager := newTestManager(t)
	manager.global = true
	project := t.TempDir()
	const count = 16
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Go(func() {
			skill := fmt.Sprintf("skill-%d", index)
			enabled, err := manager.toggle(project, skill)
			if err != nil {
				errs <- err
				return
			}
			if !enabled {
				errs <- fmt.Errorf("%s was disabled", skill)
			}
		})
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	want := make(map[string]bool, count)
	for index := range count {
		want[fmt.Sprintf("skill-%d", index)] = true
	}
	assertLock(t, manager.paths.globalLockDir, want)
}

func TestLoadLockRejectsUnsupportedSchemaRevision(t *testing.T) {
	project := t.TempDir()
	writeFile(t, filepath.Join(project, lockName), `{
  "schema_revision": 2,
  "skills": {}
}`)

	if _, err := loadLock(project); err == nil {
		t.Fatal("loadLock accepted an unsupported schema version")
	}
}

func TestLockSchemaMatchesWriterConstants(t *testing.T) {
	data, err := os.ReadFile("skills-mgr.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			SchemaRevision struct {
				Const int `json:"const"`
			} `json:"schema_revision"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties.SchemaRevision.Const != lockSchemaRevision {
		t.Fatalf(
			"schema revision = %d, want %d",
			schema.Properties.SchemaRevision.Const,
			lockSchemaRevision,
		)
	}
}

func TestListEnabledSkillsAsMarkdownWithOwnedReferenceTrees(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), `---
name: alpha
description: >
  Alpha does one thing.
  It also does another.
---
`)
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "overview.md"), "overview")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "nested", "details.md"), "details")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "ignore.txt"), "ignored")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "future.mdx"), "unknown")
	writeFile(t, filepath.Join(manager.paths.userSkills, "beta", "SKILL.md"), skillFile("beta", "Disabled.", ""))
	writeFile(t, filepath.Join(manager.paths.userSkills, "gamma", "SKILL.md"), skillFile("gamma", "No references.", ""))
	if err := saveLock(project, lock{Skills: map[string]bool{
		"alpha":  true,
		"beta":   false,
		"future": false,
		"gamma":  true,
	}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	want := `# Skill list

## alpha

Alpha does one thing. It also does another.

### references

references/
├── nested/
│   └── details.md
└── overview.md

## gamma

No references.
`
	if output.String() != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", output.String(), want)
	}
	for _, unexpected := range []string{"beta", "future", "ignore.txt", "future.mdx"} {
		if strings.Contains(output.String(), unexpected) {
			t.Fatalf("list included %q", unexpected)
		}
	}
}

func TestListEmptySelectionWritesDocumentHeading(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if want := "# Skill list\n"; output.String() != want {
		t.Fatalf("list output = %q, want %q", output.String(), want)
	}
}

func TestListRejectsMissingEnabledSkillBeforeWriting(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	if err := saveLock(project, lock{Skills: map[string]bool{
		"alpha":   true,
		"missing": true,
	}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err == nil {
		t.Fatal("list accepted a missing enabled skill")
	}
	if output.Len() != 0 {
		t.Fatalf("list wrote partial output: %q", output.String())
	}
}

func TestListRejectsMalformedEnabledSkillBeforeWriting(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "malformed", "SKILL.md"), "not frontmatter")
	if err := saveLock(project, lock{Skills: map[string]bool{"malformed": true}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err == nil {
		t.Fatal("list accepted a malformed enabled skill")
	}
	if output.Len() != 0 {
		t.Fatalf("list wrote partial output: %q", output.String())
	}
}

func TestGetSkillAndReferenceRange(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := skillFile("alpha", `"Quoted description."`, "first\nsecond\nthird\nfourth\n")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skill)
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "guide.md"), "one\ntwo\nthree\nfour\n")
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.get(project, "alpha", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "first\nsecond\nthird\nfourth\n" {
		t.Fatalf("skill Markdown output = %q", output.String())
	}

	output.Reset()
	if err := manager.get(project, "alpha/SKILL.md", "2:3", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "second\nthird\n" {
		t.Fatalf("skill Markdown range output = %q", output.String())
	}

	output.Reset()
	if err := manager.get(project, "alpha/references/guide.md", "2:3", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "two\nthree\n" {
		t.Fatalf("range output = %q", output.String())
	}
}

func TestGetStripsMarkdownFrontmatter(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha.\nfuture-key: future-value\n---\n# Body\n\nText.\n",
	)
	reference := "---\ntitle: Reference metadata\n---\n# Reference\n"
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "alpha", "references", "SKILL.md"),
		reference,
	)
	plainReference := "# Plain reference\n\nNo metadata.\n"
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "alpha", "references", "plain.md"),
		plainReference,
	)
	delimiterText := "---\nnot markdown frontmatter"
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "alpha", "references", "fixture.txt"),
		delimiterText,
	)
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.get(project, "alpha", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "# Body\n\nText.\n" {
		t.Fatalf("future frontmatter output = %q", output.String())
	}

	output.Reset()
	if err := manager.get(project, "alpha/references/SKILL.md", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "# Reference\n" {
		t.Fatalf("reference Markdown output = %q", output.String())
	}

	output.Reset()
	if err := manager.get(project, "alpha/references/plain.md", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != plainReference {
		t.Fatalf("plain Markdown output = %q", output.String())
	}

	output.Reset()
	if err := manager.get(project, "alpha/references/fixture.txt", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != delimiterText {
		t.Fatalf("non-Markdown delimiter output = %q", output.String())
	}
}

func TestGetMalformedSkillWritesNoOutput(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "malformed", "SKILL.md"),
		"---\nname: malformed\ndescription: Missing closing delimiter.\n",
	)
	if err := saveLock(project, lock{Skills: map[string]bool{"malformed": true}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.get(project, "malformed", "", &output); err == nil {
		t.Fatal("get accepted malformed skill frontmatter")
	}
	if output.Len() != 0 {
		t.Fatalf("get wrote malformed skill output: %q", output.String())
	}

	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "malformed", "references", "guide.md"),
		"---\ntitle: Missing closing delimiter.\n",
	)
	if err := manager.get(project, "malformed/references/guide.md", "", &output); err == nil {
		t.Fatal("get accepted malformed reference frontmatter")
	}
	if output.Len() != 0 {
		t.Fatalf("get wrote malformed reference output: %q", output.String())
	}
}

func TestGetRejectsDisabledAndEscapingTargets(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", ""))

	var output bytes.Buffer
	if err := manager.get(project, "unknown", "", &output); err == nil {
		t.Fatal("get accepted an unknown skill")
	}
	if err := manager.get(project, "alpha", "", &output); err == nil {
		t.Fatal("get read a disabled skill")
	}
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := manager.get(project, "alpha/../outside", "", &output); err == nil {
		t.Fatal("get accepted a path outside the skill")
	}

	outside := filepath.Join(t.TempDir(), "outside.md")
	writeFile(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(manager.paths.userSkills, "alpha", "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := manager.get(project, "alpha/escape.md", "", &output); err == nil {
		t.Fatal("get followed a symlink outside the skill")
	}
}

func TestRunSkillScripts(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(manager.paths.userSkills, "alpha")
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	interpreter := filepath.Join(t.TempDir(), "interpreter")
	writeExecutable(t, interpreter, "#!/bin/sh\nprintf 'shebang|%s|%s' \"$PWD\" \"$2\"")
	writeExecutable(t, filepath.Join(root, "scripts", "echo.sh"), "#!"+interpreter+"\n")
	writeFile(t, filepath.Join(root, "scripts", "echo.py"), "import os, sys\nprint(f'{os.getcwd()}|{sys.argv[1]}', end='')")
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"alpha/scripts/echo.sh", "alpha/scripts/echo.py"} {
		t.Run(filepath.Ext(target), func(t *testing.T) {
			if filepath.Ext(target) == ".py" {
				if _, err := exec.LookPath("python3"); err != nil {
					t.Skip("python3 is not installed")
				}
			}
			command, err := manager.scriptCommand(project, target, []string{"argument"})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			command.Stdout = &output
			if err := command.Run(); err != nil {
				t.Fatal(err)
			}
			want := root + "|argument"
			if filepath.Ext(target) == ".sh" {
				want = "shebang|" + want
			}
			if output.String() != want {
				t.Fatalf("script output = %q, want %q", output.String(), want)
			}
		})
	}
}

func TestRunJavaScriptRuntimeFallbackAndCache(t *testing.T) {
	primary := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(primary.paths.userSkills, "alpha")
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	for _, extension := range []string{".js", ".mjs", ".cjs", ".ts", ".mts", ".cts"} {
		writeFile(t, filepath.Join(root, "script"+extension), "")
	}
	if _, err := primary.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	nodeBin := t.TempDir()
	node := filepath.Join(nodeBin, "node")
	writeExecutable(t, node, "#!/bin/sh\nexit 0")
	t.Setenv("PATH", nodeBin)
	for _, extension := range []string{".js", ".mjs", ".cjs", ".ts", ".mts", ".cts"} {
		command, err := primary.scriptCommand(project, "alpha/script"+extension, nil)
		if err != nil {
			t.Fatal(err)
		}
		if command.Path != node {
			t.Fatalf("%s runtime = %q, want %q", extension, command.Path, node)
		}
	}

	bunBin := t.TempDir()
	bun := filepath.Join(bunBin, "bun")
	writeExecutable(t, bun, "#!/bin/sh\nexit 0")
	t.Setenv("PATH", bunBin)
	command, err := primary.scriptCommand(project, "alpha/script.js", nil)
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != node {
		t.Fatalf("cached runtime = %q, want %q", command.Path, node)
	}

	fallbackManager := &manager{paths: primary.paths}
	command, err = fallbackManager.scriptCommand(project, "alpha/script.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != bun {
		t.Fatalf("fallback runtime = %q, want %q", command.Path, bun)
	}
}

func TestRunJavaScriptRejectsMissingRuntimeAndUnrelatedNames(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(manager.paths.userSkills, "alpha")
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	writeFile(t, filepath.Join(root, "script.js"), "")
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "nodejs"), "#!/bin/sh\nexit 0")
	t.Setenv("PATH", bin)
	if _, err := manager.scriptCommand(project, "alpha/script.js", nil); err == nil ||
		!strings.Contains(err.Error(), "neither node nor bun") {
		t.Fatalf("missing runtime error = %v", err)
	}
}

func TestRunCommandInvokesSkillScript(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Chdir(project)
	root := filepath.Join(home, ".agents", "skills", "alpha")
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	writeExecutable(t, filepath.Join(root, "scripts", "record.sh"), "#!/bin/sh\nprintf '%s' \"$1\" > result")
	if err := saveLock(project, lock{Skills: map[string]bool{"alpha": true}}); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"run", "alpha/scripts/record.sh", "argument with spaces"}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(root, "result"), "argument with spaces")

	writeExecutable(t, filepath.Join(root, "scripts", "fail.sh"), "#!/bin/sh\nexit 23")
	err := run([]string{"run", "alpha/scripts/fail.sh"})
	exitError, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitError.ExitCode() != 23 {
		t.Fatalf("script exit error = %v, want exit code 23", err)
	}
}

func TestRunRejectsDisabledAndEscapingScripts(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(manager.paths.userSkills, "alpha")
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	writeFile(t, filepath.Join(root, "script.sh"), "exit 0")

	if _, err := manager.scriptCommand(project, "alpha/script.sh", nil); err == nil {
		t.Fatal("run accepted a script from a disabled skill")
	}
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.scriptCommand(project, "alpha/../outside.sh", nil); err == nil {
		t.Fatal("run accepted a path outside the skill")
	}
	if _, err := manager.scriptCommand(project, "alpha", nil); err == nil {
		t.Fatal("run accepted a malformed script target")
	}
	if _, err := manager.scriptCommand(project, "alpha/scripts", nil); err == nil {
		t.Fatal("run accepted a directory")
	}

	outside := filepath.Join(t.TempDir(), "outside.sh")
	writeFile(t, outside, "exit 0")
	if err := os.Symlink(outside, filepath.Join(root, "escape.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.scriptCommand(project, "alpha/escape.sh", nil); err == nil {
		t.Fatal("run followed a symlink outside the skill")
	}

	writeFile(t, filepath.Join(root, "future.jsx"), "")
	command, err := manager.scriptCommand(project, "alpha/future.jsx", nil)
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != filepath.Join(root, "future.jsx") {
		t.Fatalf("unknown extension command = %q, want direct execution", command.Path)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	writeFile(t, path, content)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runDaemon(ctx, newTestManager(t), io.Discard); err != nil {
		t.Fatal(err)
	}
}

func assertLock(t *testing.T, project string, want map[string]bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	var got lock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaRevision != lockSchemaRevision {
		t.Fatalf("lock schema revision = %d, want %d", got.SchemaRevision, lockSchemaRevision)
	}
	if len(got.Skills) != len(want) {
		t.Fatalf("lock skills = %#v, want %#v", got.Skills, want)
	}
	for name, enabled := range want {
		if got.Skills[name] != enabled {
			t.Fatalf("lock skills = %#v, want %#v", got.Skills, want)
		}
	}
}

func skillFile(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
}
