package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-yaml"
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
	if _, err := os.Lstat(filepath.Join(project, lockName+".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("toggle left a project coordination file: %v", err)
	}
	coordinationLocks, err := os.ReadDir(manager.paths.selectionLocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinationLocks) != 1 {
		t.Fatalf("coordination lock count = %d, want 1", len(coordinationLocks))
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

func TestProjectAliasesShareCoordinationLock(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	alias := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.toggle(alias, "beta"); err != nil {
		t.Fatal(err)
	}

	coordinationLocks, err := os.ReadDir(manager.paths.selectionLocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinationLocks) != 1 {
		t.Fatalf("coordination lock count = %d, want 1", len(coordinationLocks))
	}
	assertLock(t, project, map[string]bool{"alpha": true, "beta": true})
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

func TestSelectionStateLayersEnabledExpressionsWithoutEvaluatingThem(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	if err := saveLock(manager.paths.globalLockDir, lock{
		Expressions: map[string]string{"inherited": "exit 2", "project-boolean": "exit 2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, lock{
		Skills:      map[string]bool{"inherited": true},
		Expressions: map[string]string{"project-boolean": "[[ -f go.mod ]]"},
	}); err != nil {
		t.Fatal(err)
	}

	state, err := manager.selectionState(project, []discoveredSkill{
		{Name: "inherited"},
		{Name: "project-boolean"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !state.selected["inherited"] ||
		state.expressions["inherited"] != "" ||
		!state.selected["project-boolean"] ||
		state.expressions["project-boolean"] != "[[ -f go.mod ]]" {
		t.Fatalf("selection state = %#v", state)
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

func TestToggleReplacesEnabledExpressionWithBoolean(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(manager.paths.userSkills, "alpha"), "alpha")
	if err := saveLock(project, lock{
		Expressions: map[string]string{"alpha": "[[ -f go.mod ]]"},
	}); err != nil {
		t.Fatal(err)
	}

	enabled, err := manager.toggle(project, "alpha")
	if err != nil || enabled {
		t.Fatalf("toggle conditional skill = %v, %v; want disabled", enabled, err)
	}
	value, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if expression := value.Expressions["alpha"]; expression != "" {
		t.Fatalf("conditional expression was retained: %q", expression)
	}
	if current, exists := value.Skills["alpha"]; !exists || current {
		t.Fatalf("boolean selection = %#v, want false", value.Skills)
	}
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

func TestConcurrentProjectTogglesPreserveUpdates(t *testing.T) {
	manager := newTestManager(t)
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
	assertLock(t, project, want)
}

func TestLoadLegacyLockAndUpgradeOnSelectionChange(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(project, lockName), `{
  "schema_revision": 1,
  "skills": {
    "alpha": true,
    "beta": false
  }
}`)

	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	if !selected["alpha"] || selected["beta"] {
		t.Fatalf("legacy selection = %#v", selected)
	}
	if enabled, err := manager.toggle(project, "beta"); err != nil || !enabled {
		t.Fatalf("toggle legacy selection = %v, %v", enabled, err)
	}

	data, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	var current lockFile
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	alpha := current.Skills["alpha"].Enabled
	beta := current.Skills["beta"].Enabled
	if current.SchemaRevision != lockSchemaRevision ||
		alpha == nil || alpha.Boolean == nil || !*alpha.Boolean ||
		beta == nil || beta.Boolean == nil || !*beta.Boolean {
		t.Fatalf("upgraded lock = %#v", current)
	}
}

func TestLoadLockRejectsMalformedCurrentSelections(t *testing.T) {
	tests := map[string]string{
		"empty selection": `{
  "schema_revision": 2,
  "skills": {
    "alpha": {}
  }
}`,
		"null selection": `{
  "schema_revision": 2,
  "skills": {
    "alpha": null
  }
}`,
		"incomplete remote": `{
  "schema_revision": 2,
  "skills": {
    "alpha": {
      "enabled": false,
      "remote": {
        "provider": "skills.sh"
      }
    }
  }
}`,
		"empty expression": `{
  "schema_revision": 3,
  "skills": {
    "alpha": {
      "enabled": "   "
    }
  }
}`,
		"numeric enabled": `{
  "schema_revision": 3,
  "skills": {
    "alpha": {
      "enabled": 1
    }
  }
}`,
		"null enabled": `{
  "schema_revision": 3,
  "skills": {
    "alpha": {
      "enabled": null
    }
  }
}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, filepath.Join(project, lockName), contents)
			if _, err := loadLock(project); err == nil {
				t.Fatal("loadLock accepted malformed current selection")
			}
		})
	}
}

func TestLockRoundTripsEnabledExpression(t *testing.T) {
	project := t.TempDir()
	want := "has_dependency tauri '>=2' && printf '<script>&'"
	if err := saveLock(project, lock{
		Expressions: map[string]string{"go-review": want},
		Remote: map[string]remoteSkillRef{
			"go-review": {
				Provider: skillsShProvider,
				ID:       "owner/repo/go-review",
				Name:     "go-review",
				Locator:  "owner/repo/go-review",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaRevision != lockSchemaRevision ||
		got.Expressions["go-review"] != want ||
		got.Remote["go-review"].Name != "go-review" {
		t.Fatalf("round-tripped lock = %#v", got)
	}
	data, err := os.ReadFile(filepath.Join(project, lockName))
	if err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(data, []byte(escaped)) {
			t.Fatalf("selection escaped %q:\n%s", escaped, data)
		}
	}
	if !bytes.Contains(data, []byte(want)) {
		t.Fatalf("selection omitted readable expression:\n%s", data)
	}
}

func TestLoadPreviousLockAndUpgradeOnSelectionChange(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(project, lockName), `{
  "schema_revision": 2,
  "skills": {
    "alpha": {
      "enabled": true
    }
  }
}`)

	if enabled, err := manager.toggle(project, "alpha"); err != nil || enabled {
		t.Fatalf("toggle previous selection = %v, %v", enabled, err)
	}
	got, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaRevision != lockSchemaRevision || got.Skills["alpha"] {
		t.Fatalf("upgraded lock = %#v", got)
	}
}

func TestLoadLockRejectsUnsupportedSchemaRevision(t *testing.T) {
	project := t.TempDir()
	writeFile(t, filepath.Join(project, lockName), `{
  "schema_revision": 4,
  "skills": {}
}`)

	if _, err := loadLock(project); err == nil {
		t.Fatal("loadLock accepted an unsupported schema version")
	}
}

func TestLockSchemaMatchesSupportedRevisions(t *testing.T) {
	data, err := os.ReadFile("skills-mgr.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions struct {
			Legacy struct {
				Properties struct {
					SchemaRevision struct {
						Const int `json:"const"`
					} `json:"schema_revision"`
				} `json:"properties"`
			} `json:"legacyLock"`
			Previous struct {
				Properties struct {
					SchemaRevision struct {
						Const int `json:"const"`
					} `json:"schema_revision"`
				} `json:"properties"`
			} `json:"previousLock"`
			Current struct {
				Properties struct {
					SchemaRevision struct {
						Const int `json:"const"`
					} `json:"schema_revision"`
				} `json:"properties"`
			} `json:"currentLock"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Definitions.Legacy.Properties.SchemaRevision.Const !=
		legacyLockSchemaRevision ||
		schema.Definitions.Previous.Properties.SchemaRevision.Const !=
			previousLockSchemaRevision ||
		schema.Definitions.Current.Properties.SchemaRevision.Const !=
			lockSchemaRevision {
		t.Fatalf("schema revisions = %#v", schema.Definitions)
	}
}

func TestListEnabledSkillsAsXMLWithOwnedReferences(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), `---
name: alpha
description: >
  Alpha's description does <one> & one thing.
  It also does "another".
---
`)
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "NOTES.md"), "notes")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", ".hidden.md"), "hidden")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "_private.md"), "private")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "ignore.md.txt"), "ignored")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "escape&me.md"), "escaped")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "overview.md"), "overview")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "nested", "details.md"), "details")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", ".secret.md"), "secret")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "_draft.md"), "draft")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "_internal", "notes.md"), "internal")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", ".cache", "data.md"), "cache")
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
	want := `<skills>
  <skill name="alpha" description="Alpha&#39;s description does &lt;one&gt; &amp; one thing. It also does &#34;another&#34;.">
    <references>NOTES.md
references/
  escape&amp;me.md
  nested/details.md
  overview.md</references>
  </skill>
  <skill name="gamma" description="No references."></skill>
</skills>
`
	if output.String() != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", output.String(), want)
	}
	for _, unexpected := range []string{
		"beta",
		"future",
		"ignore.txt",
		"ignore.md.txt",
		"future.mdx",
		"SKILL.md",
		".hidden.md",
		"_private.md",
		".secret.md",
		"_draft.md",
		"_internal",
		".cache",
	} {
		if strings.Contains(output.String(), unexpected) {
			t.Fatalf("list included %q", unexpected)
		}
	}
}

func TestListOmitsHiddenAndUnderscorePrefixedReferences(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skillFile("alpha", "No public references.", ""))
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", ".hidden.md"), "hidden")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "_private.md"), "private")
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "references", "_draft.md"), "draft")
	if err := saveLock(project, lock{Skills: map[string]bool{"alpha": true}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	want := `<skills>
  <skill name="alpha" description="No public references."></skill>
</skills>
`
	if output.String() != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", output.String(), want)
	}
}

func TestListKeepsFullSkillDescription(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	description := "Use this skill when matching long instructions. " + strings.Repeat("more ", 80) + "END-MARKER."
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skillFile("alpha", description, ""))
	if err := saveLock(project, lock{Skills: map[string]bool{"alpha": true}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	listed := output.String()
	if !strings.Contains(listed, `description="`+description+`"`) {
		t.Fatalf("list omitted the full description:\n%s", listed)
	}
	if !strings.Contains(listed, "END-MARKER.") {
		t.Fatalf("list truncated the description:\n%s", listed)
	}
}

func TestListHidesModelInvocationDisabledSkillButGetAllowsIt(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "manual", "SKILL.md"),
		"---\nname: manual\ndescription: Invoke only when explicitly requested.\ndisable-model-invocation: true\n---\nmanual body\n",
	)
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "automatic", "SKILL.md"),
		skillFile("automatic", "Available for model invocation.", "automatic body\n"),
	)
	if err := saveLock(project, lock{Skills: map[string]bool{
		"automatic": true,
	}}); err != nil {
		t.Fatal(err)
	}

	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	if !selected["manual"] {
		t.Fatalf("selection = %#v, want manual enabled by default", selected)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `name="manual"`) {
		t.Fatalf("list exposed model-invocation-disabled skill:\n%s", output.String())
	}
	if !strings.Contains(output.String(), `name="automatic"`) {
		t.Fatalf("list omitted model-invocable skill:\n%s", output.String())
	}

	output.Reset()
	if err := manager.get(project, "manual", "", &output); err != nil {
		t.Fatal(err)
	}
	if want := "manual body\n"; output.String() != want {
		t.Fatalf("manual skill output = %q, want %q", output.String(), want)
	}

	enabled, err := manager.toggle(project, "manual")
	if err != nil || enabled {
		t.Fatalf("toggle default-enabled manual skill = %v, %v; want disabled", enabled, err)
	}
	assertLock(t, project, map[string]bool{"automatic": true, "manual": false})
	output.Reset()
	if err := manager.get(project, "manual", "", &output); err == nil {
		t.Fatal("get read an explicitly disabled manual-only skill")
	}
}

func TestEnabledExpressionControlsListGetAndRun(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	root := filepath.Join(project, ".agents", "skills", "go-review")
	writeFile(
		t,
		filepath.Join(root, "SKILL.md"),
		skillFile("go-review", "Review Go code.", "body\n"),
	)
	writeExecutable(t, filepath.Join(root, "check.sh"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SKILLS_MGR_TEST_ENABLED", "yes")
	if err := saveLock(project, lock{
		Expressions: map[string]string{
			"go-review": "[[ -f go.mod && $SKILLS_MGR_TEST_ENABLED == yes ]]",
		},
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `name="go-review"`) {
		t.Fatalf("false expression listed skill:\n%s", output.String())
	}
	if err := manager.get(project, "go-review", "", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("get error = %v, want disabled", err)
	}
	if _, err := manager.scriptCommand(project, "go-review/check.sh", nil); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("run error = %v, want disabled", err)
	}

	writeFile(t, filepath.Join(project, "go.mod"), "module example.com/project\n")
	output.Reset()
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `name="go-review"`) {
		t.Fatalf("true expression omitted skill:\n%s", output.String())
	}
	if err := manager.get(project, "go-review", "", io.Discard); err != nil {
		t.Fatalf("get with true expression: %v", err)
	}
	if _, err := manager.scriptCommand(project, "go-review/check.sh", nil); err != nil {
		t.Fatalf("run with true expression: %v", err)
	}
}

func TestEnabledExpressionUsesStatusOneForFalseAndHigherStatusForError(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(project, ".agents", "skills", "alpha", "SKILL.md"),
		skillFile("alpha", "Alpha.", "body\n"),
	)

	for _, test := range []struct {
		name       string
		expression string
		wantError  string
	}{
		{name: "false", expression: "exit 1"},
		{name: "status two", expression: "exit 2", wantError: "status 2"},
		{name: "parse error", expression: "[[", wantError: "parse enabled expression"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := saveLock(project, lock{
				Expressions: map[string]string{"alpha": test.expression},
			}); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := manager.list(project, &output)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(output.String(), `name="alpha"`) {
					t.Fatalf("false expression listed skill:\n%s", output.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("list error = %v, want %q", err, test.wantError)
			}
			if output.Len() != 0 {
				t.Fatalf("failed list wrote partial XML: %q", output.String())
			}
		})
	}
}

func TestEnabledExpressionHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := evaluateEnabled(ctx, t.TempDir(), "alpha", "while true; do :; done")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("evaluateEnabled error = %v, want canceled context", err)
	}
}

func TestToggleModelInvocationFrontmatterPreservesMetadataAndBody(t *testing.T) {
	original := []byte("\xEF\xBB\xBF---\r\n" +
		"# keep this comment\r\n" +
		"name: alpha\r\n" +
		"description: Alpha.\r\n" +
		"metadata:\r\n" +
		"  owner: team # keep inline\r\n" +
		"\"disable-model-invocation\": false # keep policy comment\r\n" +
		"---\r\n" +
		"# Body\r\n\r\nDo not rewrite this body.\r\n")
	updated, disabled, err := toggleModelInvocationFrontmatter(original)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled {
		t.Fatal("toggle did not disable model invocation")
	}
	for _, preserved := range [][]byte{
		[]byte("\xEF\xBB\xBF---\r\n"),
		[]byte("# keep this comment\r\n"),
		[]byte("metadata:\r\n  owner: team # keep inline\r\n"),
		[]byte("# keep policy comment\r\n"),
		[]byte("---\r\n# Body\r\n\r\nDo not rewrite this body.\r\n"),
	} {
		if !bytes.Contains(updated, preserved) {
			t.Fatalf("updated frontmatter lost %q:\n%s", preserved, updated)
		}
	}
	if !bytes.Contains(updated, []byte("disable-model-invocation\": true")) {
		t.Fatalf("updated frontmatter did not set true:\n%s", updated)
	}
}

func TestToggleModelInvocationFrontmatterPreservesYAMLStyles(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		oldValue    string
		newValue    string
	}{
		{
			name: "anchor and alias",
			frontmatter: "name: alpha\n" +
				"description: Alpha.\n" +
				"disable-model-invocation: &model_policy false\n" +
				"metadata:\n  policy-copy: *model_policy\n",
			oldValue: "&model_policy false",
			newValue: "&model_policy true",
		},
		{
			name: "tagged boolean",
			frontmatter: "name: alpha\n" +
				"description: Alpha.\n" +
				"disable-model-invocation: !!bool false\n",
			oldValue: "!!bool false",
			newValue: "!!bool true",
		},
		{
			name: "multiline quoted value",
			frontmatter: "name: alpha\n" +
				"description: \"Alpha has a long\\n  description.\"\n" +
				"disable-model-invocation: false\n",
		},
		{
			name: "block scalar with trailing blank lines",
			frontmatter: "name: alpha\n" +
				"description: Alpha.\n" +
				"notes: |+\n  first line\n\n\n" +
				"disable-model-invocation: false\n",
		},
		{
			name: "compact flow values",
			frontmatter: "name: alpha\n" +
				"description: Alpha.\n" +
				"allowed-tools: [Read,Write]\n" +
				"metadata: {owner:team,tags:[one,two]}\n" +
				"disable-model-invocation: false\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte("---\n" + tt.frontmatter + "---\nbody\n")
			updated, disabled, err := toggleModelInvocationFrontmatter(original)
			if err != nil {
				t.Fatal(err)
			}
			if !disabled {
				t.Fatal("toggle did not disable model invocation")
			}
			oldValue := cmp.Or(tt.oldValue, "false")
			newValue := cmp.Or(tt.newValue, "true")
			want := bytes.Replace(
				original,
				[]byte("disable-model-invocation: "+oldValue),
				[]byte("disable-model-invocation: "+newValue),
				1,
			)
			if !bytes.Equal(updated, want) {
				t.Fatalf("toggle changed unrelated YAML:\ngot:\n%s\nwant:\n%s", updated, want)
			}
			frontmatter, _, status, err := readFrontmatter(bytes.NewReader(updated))
			if err != nil || status != frontmatterValid {
				t.Fatalf("updated frontmatter status = %v, error = %v", status, err)
			}
			var decoded map[string]any
			if err := yaml.Unmarshal([]byte(frontmatter), &decoded); err != nil {
				t.Fatalf("updated frontmatter is invalid: %v", err)
			}
		})
	}
}

func TestToggleModelInvocationFrontmatterRejectsAliasPolicy(t *testing.T) {
	original := []byte("---\n" +
		"name: alpha\n" +
		"description: Alpha.\n" +
		"metadata:\n  policy: &model_policy false\n" +
		"disable-model-invocation: *model_policy\n" +
		"---\nbody\n")
	updated, _, err := toggleModelInvocationFrontmatter(original)
	if err == nil || !strings.Contains(err.Error(), "must be a boolean value") {
		t.Fatalf("alias policy error = %v", err)
	}
	if updated != nil {
		t.Fatalf("alias policy returned modified content:\n%s", updated)
	}
}

func TestToggleModelInvocationFrontmatterAddsPolicyToFlowMapping(t *testing.T) {
	original := []byte("---\n{name: alpha,description: Alpha.,metadata: {tags:[one,two]}} # retain } literally\n---\nbody\n")
	updated, disabled, err := toggleModelInvocationFrontmatter(original)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled {
		t.Fatal("toggle did not disable model invocation")
	}
	want := []byte("---\n{name: alpha,description: Alpha.,metadata: {tags:[one,two]}, disable-model-invocation: true} # retain } literally\n---\nbody\n")
	if !bytes.Equal(updated, want) {
		t.Fatalf("flow-style toggle:\ngot:\n%s\nwant:\n%s", updated, want)
	}
}

func TestToggleModelInvocationFrontmatterSupportsTaggedBooleanSpellings(t *testing.T) {
	tests := []struct {
		name     string
		original string
		want     string
		disabled bool
	}{
		{name: "lowercase yes", original: "yes", want: "no", disabled: false},
		{name: "uppercase no", original: "NO", want: "YES", disabled: true},
		{name: "mixed case yes", original: "yEs", want: "no", disabled: false},
		{name: "numeric true", original: "1", want: "0", disabled: false},
		{name: "numeric false", original: "0", want: "1", disabled: true},
		{name: "uppercase short true", original: "T", want: "F", disabled: false},
		{name: "lowercase short false", original: "f", want: "t", disabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte("---\nname: alpha\ndescription: Alpha.\n" +
				"disable-model-invocation: !!bool " + tt.original + "\n---\nbody\n")
			updated, disabled, err := toggleModelInvocationFrontmatter(original)
			if err != nil {
				t.Fatal(err)
			}
			if disabled != tt.disabled {
				t.Fatalf("disabled = %v, want %v", disabled, tt.disabled)
			}
			want := bytes.Replace(
				original,
				[]byte("!!bool "+tt.original),
				[]byte("!!bool "+tt.want),
				1,
			)
			if !bytes.Equal(updated, want) {
				t.Fatalf("toggle changed unrelated YAML:\ngot:\n%s\nwant:\n%s", updated, want)
			}
		})
	}
}

func TestToggleModelInvocationUpdatesEditableSkillAndSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Alpha.", "body\n"))
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	discovered, err := manager.findSkill(project, "alpha")
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.toggleModelInvocation(t.Context(), project, discovered)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Disabled || !result.Selected["alpha"] ||
		len(result.Skills) != 1 || !result.Skills[0].DisableModelInvocation {
		t.Fatalf("disabled result = %#v", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("skill mode = %o, want 755", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("---\nbody\n")) {
		t.Fatalf("toggle changed skill body:\n%s", data)
	}

	result, err = manager.toggleModelInvocation(t.Context(), project, result.Skills[0])
	if err != nil {
		t.Fatal(err)
	}
	if result.Disabled || result.Selected["alpha"] ||
		result.Skills[0].DisableModelInvocation {
		t.Fatalf("enabled result = %#v", result)
	}
}

func TestToggleModelInvocationSerializesLocalUpdates(t *testing.T) {
	manager := newTestManager(t)
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Alpha.", "body\n"))
	type toggleResult struct {
		disabled bool
		err      error
	}
	results := make(chan toggleResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			disabled, err := toggleModelInvocationFile(
				t.Context(),
				manager.paths.selectionLocks,
				path,
			)
			results <- toggleResult{disabled: disabled, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	states := make([]bool, 0, 2)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		states = append(states, result.disabled)
	}
	if len(states) != 2 || !slices.Contains(states, false) || !slices.Contains(states, true) {
		t.Fatalf("serialized local toggle states = %v", states)
	}
	discovered, ok, err := parseSkill(path)
	if err != nil || !ok {
		t.Fatalf("parse final skill = %#v, %v, %v", discovered, ok, err)
	}
	if discovered.DisableModelInvocation {
		t.Fatalf("two local toggles did not restore the original state: %#v", discovered)
	}
}

func TestToggleModelInvocationRejectsReadOnlySkill(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(manager.paths.adminSkills, "alpha", "SKILL.md"),
		skillFile("alpha", "Alpha.", "body"),
	)
	discovered, err := manager.findSkill(project, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.toggleModelInvocation(t.Context(), project, discovered)
	if err == nil || !strings.Contains(err.Error(), "not editable") {
		t.Fatalf("toggle error = %v, want non-editable source", err)
	}
}

func TestListEmptySelectionWritesXMLDocument(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if want := "<skills></skills>\n"; output.String() != want {
		t.Fatalf("list output = %q, want %q", output.String(), want)
	}
}

func TestListFiltersSkillsVisibleToHarness(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	if _, err := manager.remoteStore.ensure(
		t.Context(),
		remoteSkillRef{
			Provider: skillsShProvider,
			ID:       "owner/repo/remote-shared",
			Name:     "remote-shared",
			Locator:  "owner/repo/remote-shared",
		},
		&staticRemoteProvider{files: []remoteSkillFile{{
			Path:     "SKILL.md",
			Contents: []byte(skillFile("remote-shared", "Remote skill not installed for a harness.", "body")),
		}}},
	); err != nil {
		t.Fatal(err)
	}
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "common", "SKILL.md"),
		skillFile("common", "Shared agents skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(project, ".agents", "skills", "local", "SKILL.md"),
		skillFile("local", "Project agents skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "claude-shared", "SKILL.md"),
		skillFile("claude-shared", "Common skill also installed for Claude.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.claudeSkills, "claude-shared", "SKILL.md"),
		skillFile("claude-shared", "Claude-native copy.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "grok-shared", "SKILL.md"),
		skillFile("grok-shared", "Common skill also installed for Grok.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.grokSkills, "grok-shared", "SKILL.md"),
		skillFile("grok-shared", "Grok-native copy.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.codexHome, "skills", "codex-only", "SKILL.md"),
		skillFile("codex-only", "Codex-only skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.codexHome, "plugins", "cache", "publisher", "plugin", "hash", "skills", "plugin-only", "SKILL.md"),
		skillFile("plugin-only", "Codex plugin skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.adminSkills, "admin-only", "SKILL.md"),
		skillFile("admin-only", "Codex admin skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(manager.paths.codexHome, "skills", ".system", "builtin", "SKILL.md"),
		skillFile("builtin", "Codex bundled skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(project, ".claude", "skills", "project-claude", "SKILL.md"),
		skillFile("project-claude", "Project Claude skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(project, ".grok", "skills", "project-grok", "SKILL.md"),
		skillFile("project-grok", "Project Grok skill.", ""),
	)
	writeFile(
		t,
		filepath.Join(project, ".codex", "skills", "project-codex", "SKILL.md"),
		skillFile("project-codex", "Project Codex skill.", ""),
	)
	if err := saveLock(project, lock{Skills: map[string]bool{
		"admin-only":     true,
		"builtin":        true,
		"claude-shared":  true,
		"codex-only":     true,
		"common":         true,
		"grok-shared":    true,
		"plugin-only":    true,
		"project-claude": true,
		"project-codex":  true,
		"project-grok":   true,
		"remote-shared":  true,
	}}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		harnesses []listHarness
		wantNames []string
		omitNames []string
	}{
		{
			name:      "unscoped",
			wantNames: []string{"admin-only", "builtin", "claude-shared", "codex-only", "common", "grok-shared", "local", "plugin-only", "project-claude", "project-codex", "project-grok", "remote-shared"},
		},
		{
			name:      "claude",
			harnesses: []listHarness{listHarnessClaude},
			wantNames: []string{"common", "grok-shared", "local", "remote-shared"},
			omitNames: []string{"admin-only", "builtin", "claude-shared", "codex-only", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
		{
			name:      "grok",
			harnesses: []listHarness{listHarnessGrok},
			wantNames: []string{"remote-shared"},
			omitNames: []string{"admin-only", "builtin", "claude-shared", "codex-only", "common", "grok-shared", "local", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
		{
			name:      "claude and grok",
			harnesses: []listHarness{listHarnessClaude, listHarnessGrok},
			wantNames: []string{"remote-shared"},
			omitNames: []string{"admin-only", "builtin", "claude-shared", "codex-only", "common", "grok-shared", "local", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
		{
			name:      "codex",
			harnesses: []listHarness{listHarnessCodex},
			wantNames: []string{"claude-shared", "common", "grok-shared", "local", "remote-shared"},
			omitNames: []string{"admin-only", "builtin", "codex-only", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := manager.list(project, &output, tt.harnesses...); err != nil {
				t.Fatal(err)
			}
			for _, name := range tt.wantNames {
				if !strings.Contains(output.String(), `name="`+name+`"`) {
					t.Errorf("list omitted %q:\n%s", name, output.String())
				}
			}
			for _, name := range tt.omitNames {
				if strings.Contains(output.String(), `name="`+name+`"`) {
					t.Errorf("list included out-of-scope skill %q:\n%s", name, output.String())
				}
			}
		})
	}
}

func TestListOmitsAgentsSkillsAlreadyVisibleToGrok(t *testing.T) {
	manager := newTestManager(t)
	home := manager.paths.globalLockDir
	writeFile(
		t,
		filepath.Join(manager.paths.userSkills, "user-skill", "SKILL.md"),
		skillFile("user-skill", "User agents skill.", ""),
	)
	if err := saveLock(home, lock{Skills: map[string]bool{"user-skill": true}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(home, &output, listHarnessGrok); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `name="user-skill"`) {
		t.Fatalf("list --grok from home included a user agents skill:\n%s", output.String())
	}

	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(project, ".agents", "skills", "local-skill", "SKILL.md"),
		skillFile("local-skill", "Project agents skill.", ""),
	)
	output.Reset()
	if err := manager.list(project, &output, listHarnessGrok); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `name="local-skill"`) {
		t.Fatalf("list --grok included a project agents skill:\n%s", output.String())
	}
	if strings.Contains(output.String(), `name="user-skill"`) {
		t.Fatalf("list --grok included a user agents skill from a project:\n%s", output.String())
	}

	linked := t.TempDir()
	writeFile(
		t,
		filepath.Join(linked, ".agents", "skills", "linked-skill", "SKILL.md"),
		skillFile("linked-skill", "Agents skill also exposed through a grok symlink.", ""),
	)
	if err := os.MkdirAll(filepath.Join(linked, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(linked, ".agents", "skills"),
		filepath.Join(linked, ".grok", "skills"),
	); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := manager.list(linked, &output, listHarnessGrok); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `name="linked-skill"`) {
		t.Fatalf("list --grok included a skill visible through a grok skills symlink:\n%s", output.String())
	}
}

func TestGetAndRunResolveSkillsFromAnyAgentSource(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(manager.paths.codexHome, "skills", "codex-only", "SKILL.md"),
		skillFile("codex-only", "Codex-only skill.", "codex body\n"),
	)
	writeExecutable(
		t,
		filepath.Join(manager.paths.codexHome, "skills", "codex-only", "scripts", "echo.sh"),
		"#!/bin/sh\nprintf codex\n",
	)
	writeFile(
		t,
		filepath.Join(project, ".claude", "skills", "project-claude", "SKILL.md"),
		skillFile("project-claude", "Project Claude skill.", "claude body\n"),
	)
	if err := saveLock(project, lock{Skills: map[string]bool{
		"codex-only":     true,
		"project-claude": true,
	}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.get(project, "codex-only", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "codex body\n" {
		t.Fatalf("Codex skill output = %q", output.String())
	}

	command, err := manager.scriptCommand(project, "codex-only/scripts/echo.sh", nil)
	if err != nil {
		t.Fatal(err)
	}
	scriptOutput, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(scriptOutput) != "codex" {
		t.Fatalf("Codex script output = %q", scriptOutput)
	}

	output.Reset()
	if err := manager.get(project, "project-claude", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "claude body\n" {
		t.Fatalf("Claude skill output = %q", output.String())
	}
}

func TestListHarnessFilteringChoosesRemoteSkillOverOtherAgentSkill(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	const name = "writing-for-agents"
	if _, err := manager.remoteStore.ensure(
		t.Context(),
		remoteSkillRef{
			Provider: skillsShProvider,
			ID:       "owner/repo/" + name,
			Name:     name,
			Locator:  "owner/repo/" + name,
		},
		&staticRemoteProvider{files: []remoteSkillFile{{
			Path:     "SKILL.md",
			Contents: []byte(skillFile(name, "Remote skill.", "remote body\n")),
		}}},
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.setRemotePlaceholders(
		project,
		name,
		"name: "+name+"\ndescription: Remote skill.\n",
		true,
	); err != nil {
		t.Fatal(err)
	}
	writeFile(
		t,
		filepath.Join(manager.paths.codexHome, "skills", name, "SKILL.md"),
		skillFile(name, "Codex skill with the same name.", "codex body\n"),
	)
	if err := saveLock(project, lock{Skills: map[string]bool{name: true}}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output, listHarnessClaude); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `name="`+name+`"`) {
		t.Fatalf("Claude list omitted remote skill shadowed by a Codex skill:\n%s", output.String())
	}
}

func TestParseHarnessArgs(t *testing.T) {
	harnesses, rest, err := parseHarnessArgs([]string{"--claude", "--grok", "--codex"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(harnesses, []listHarness{listHarnessClaude, listHarnessGrok, listHarnessCodex}) {
		t.Fatalf("harnesses = %v", harnesses)
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %v, want empty", rest)
	}

	harnesses, rest, err = parseHarnessArgs([]string{"--grok", "alpha", "1:2"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(harnesses, []listHarness{listHarnessGrok}) {
		t.Fatalf("harnesses = %v", harnesses)
	}
	if !slices.Equal(rest, []string{"alpha", "1:2"}) {
		t.Fatalf("rest = %v", rest)
	}

	if _, _, err := parseHarnessArgs([]string{"--unknown"}); err == nil {
		t.Fatal("parseHarnessArgs accepted an unknown flag")
	}
}

func clearHarnessEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDECODE", "")
	t.Setenv("GROK_AGENT", "")
	t.Setenv("GROK_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
}

func TestInferHarnessFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []listHarness
	}{
		{name: "none"},
		{
			name: "claude config is not a session",
			env: map[string]string{
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				"CLAUDE_PACKAGE_MANAGER":                   "bun",
			},
		},
		{
			name: "claude",
			env:  map[string]string{"CLAUDECODE": "1"},
			want: []listHarness{listHarnessClaude},
		},
		{
			name: "grok agent",
			env:  map[string]string{"GROK_AGENT": "1"},
			want: []listHarness{listHarnessGrok},
		},
		{
			name: "grok session",
			env:  map[string]string{"GROK_SESSION_ID": "sess"},
			want: []listHarness{listHarnessGrok},
		},
		{
			name: "grok agent and session",
			env: map[string]string{
				"GROK_AGENT":      "1",
				"GROK_SESSION_ID": "sess",
			},
			want: []listHarness{listHarnessGrok},
		},
		{
			name: "codex",
			env:  map[string]string{"CODEX_THREAD_ID": "thread"},
			want: []listHarness{listHarnessCodex},
		},
		{
			name: "multiple sessions stay unscoped",
			env: map[string]string{
				"GROK_AGENT":      "1",
				"CODEX_THREAD_ID": "thread",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearHarnessEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			got := inferHarnessFromEnv()
			if !slices.Equal(got, tt.want) {
				t.Fatalf("inferHarnessFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveHarnessesPrefersFlags(t *testing.T) {
	clearHarnessEnv(t)
	t.Setenv("GROK_AGENT", "1")
	t.Setenv("GROK_SESSION_ID", "sess")

	got := resolveHarnesses([]listHarness{listHarnessClaude})
	if !slices.Equal(got, []listHarness{listHarnessClaude}) {
		t.Fatalf("explicit flags = %v, want claude", got)
	}

	got = resolveHarnesses(nil)
	if !slices.Equal(got, []listHarness{listHarnessGrok}) {
		t.Fatalf("inferred = %v, want grok", got)
	}
}

func TestProjectSkillDefaultsToEnabled(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(
		t,
		filepath.Join(project, ".agents", "skills", "local", "SKILL.md"),
		skillFile("local", "Repository-local skill.", "local body\n"),
	)

	selected, err := manager.selection(project)
	if err != nil {
		t.Fatal(err)
	}
	if !selected["local"] {
		t.Fatalf("selection = %#v, want local enabled", selected)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `name="local"`) {
		t.Fatalf("list omitted default-enabled project skill:\n%s", output.String())
	}

	output.Reset()
	if err := manager.get(project, "local", "", &output); err != nil {
		t.Fatal(err)
	}
	if want := "local body\n"; output.String() != want {
		t.Fatalf("skill output = %q, want %q", output.String(), want)
	}

	enabled, err := manager.toggle(project, "local")
	if err != nil || enabled {
		t.Fatalf("toggle default-enabled project skill = %v, %v; want disabled", enabled, err)
	}
	assertLock(t, project, map[string]bool{"local": false})

	output.Reset()
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if want := "<skills></skills>\n"; output.String() != want {
		t.Fatalf("disabled list output = %q, want %q", output.String(), want)
	}
	if err := manager.get(project, "local", "", &output); err == nil {
		t.Fatal("get read an explicitly disabled project skill")
	}
}

func TestListSkipsDeletedGloballyEnabledSkillWithoutChangingSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	deletedRoot := filepath.Join(manager.paths.userSkills, "deleted")
	writeFile(t, filepath.Join(deletedRoot, "SKILL.md"), skillFile("deleted", "Deleted.", ""))
	selected := map[string]bool{
		"alpha":   true,
		"deleted": true,
	}
	if err := saveLock(manager.paths.globalLockDir, lock{Skills: selected}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(deletedRoot); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if want := "<skills>\n  <skill name=\"alpha\" description=\"Alpha.\"></skill>\n</skills>\n"; output.String() != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", output.String(), want)
	}
	assertLock(t, manager.paths.globalLockDir, selected)
}

func TestListSkipsMalformedEnabledSkillWithoutChangingSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", ""))
	writeFile(t, filepath.Join(manager.paths.userSkills, "malformed", "SKILL.md"), "not frontmatter")
	selected := map[string]bool{
		"alpha":     true,
		"malformed": true,
	}
	if err := saveLock(project, lock{Skills: selected}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	if want := "<skills>\n  <skill name=\"alpha\" description=\"Alpha.\"></skill>\n</skills>\n"; output.String() != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", output.String(), want)
	}
	assertLock(t, project, selected)
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

func TestGetAndRunCommandsIgnoreHarnessScope(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Chdir(project)
	root := filepath.Join(home, ".codex", "skills", "codex-only")
	writeFile(
		t,
		filepath.Join(root, "SKILL.md"),
		skillFile("codex-only", "Codex-only skill.", "codex body\n"),
	)
	writeExecutable(
		t,
		filepath.Join(root, "scripts", "record.sh"),
		"#!/bin/sh\nprintf codex > result\n",
	)
	if err := saveLock(project, lock{Skills: map[string]bool{"codex-only": true}}); err != nil {
		t.Fatal(err)
	}

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
	getErr := run([]string{"get", "--claude", "codex-only"})
	os.Stdout = originalStdout
	if closeErr := stdout.Close(); getErr == nil {
		getErr = closeErr
	}
	if getErr != nil {
		t.Fatal(getErr)
	}
	assertFile(t, stdout.Name(), "codex body\n")

	if err := run([]string{"run", "--claude", "codex-only/scripts/record.sh"}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(root, "result"), "codex")
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

func assertLock(t *testing.T, project string, want map[string]bool) {
	t.Helper()
	got, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
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
