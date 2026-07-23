package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToggleUpdatesOnlyProjectLock(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := filepath.Join(manager.paths.library, "alpha", "SKILL.md")
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

func TestListEnabledSkillsWithReferenceTree(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), `---
name: alpha
description: >
  Alpha does one thing.
  It also does another.
---
`)
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "references", "overview.md"), "overview")
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "references", "nested", "details.md"), "details")
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "references", "ignore.txt"), "ignored")
	writeFile(t, filepath.Join(manager.paths.library, "beta", "SKILL.md"), skillFile("beta", "Disabled.", ""))
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.list(project, &output); err != nil {
		t.Fatal(err)
	}
	want := `alpha — Alpha does one thing. It also does another.
└── references/
    ├── nested/
    │   └── details.md
    └── overview.md
`
	if output.String() != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", output.String(), want)
	}
	if strings.Contains(output.String(), "beta") {
		t.Fatal("list included a disabled skill")
	}
}

func TestListRejectsMissingEnabledSkillBeforeWriting(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", ""))
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

func TestGetSkillAndReferenceRange(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := skillFile("alpha", `"Quoted description."`, "first\nsecond\nthird\nfourth\n")
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), skill)
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "references", "guide.md"), "one\ntwo\nthree\nfour\n")
	if _, err := manager.toggle(project, "alpha"); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := manager.get(project, "alpha", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != skill {
		t.Fatalf("skill output = %q, want %q", output.String(), skill)
	}

	output.Reset()
	if err := manager.get(project, "alpha/references/guide.md", "2:3", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "two\nthree\n" {
		t.Fatalf("range output = %q", output.String())
	}
}

func TestGetRejectsDisabledAndEscapingTargets(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.library, "alpha", "SKILL.md"), skillFile("alpha", "Alpha.", ""))

	var output bytes.Buffer
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
	if err := os.Symlink(outside, filepath.Join(manager.paths.library, "alpha", "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := manager.get(project, "alpha/escape.md", "", &output); err == nil {
		t.Fatal("get followed a symlink outside the skill")
	}
}

func TestDaemonStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runDaemon(ctx); err != nil {
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
