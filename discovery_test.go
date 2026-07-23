package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverSkillsFromStandardRoots(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(project, ".agents", "skills", "project"), "project-skill")
	writeSkill(t, filepath.Join(manager.paths.userSkills, "user"), "user-skill")
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", "codex"), "codex-skill")
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", ".system", "system"), "system-skill")
	writeSkill(t, filepath.Join(manager.paths.adminSkills, "admin"), "admin-skill")
	writeSkill(
		t,
		filepath.Join(manager.paths.codexHome, "plugins", "cache", "publisher", "plugin", "hash", "skills", "plugin"),
		"plugin-skill",
	)
	writeSkill(
		t,
		filepath.Join(manager.paths.userSkills, "user", "references", "nested"),
		"nested-skill",
	)

	skills, err := manager.skills(project)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(skills))
	sources := make(map[string]string, len(skills))
	for _, skill := range skills {
		names = append(names, skill.Name)
		sources[skill.Name] = skill.Source
	}
	want := []string{
		"admin-skill",
		"codex-skill",
		"plugin-skill",
		"project-skill",
		"system-skill",
		"user-skill",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("skills = %q, want %q", names, want)
	}
	wantSources := map[string]string{
		"admin-skill":   "admin",
		"codex-skill":   "codex",
		"plugin-skill":  "plugin",
		"project-skill": "project",
		"system-skill":  "bundled",
		"user-skill":    "user",
	}
	for name, wantSource := range wantSources {
		if sources[name] != wantSource {
			t.Errorf("source of %s = %q, want %q", name, sources[name], wantSource)
		}
	}
}

func TestDiscoverSkillsUsesSourcePrecedence(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	projectRoot := filepath.Join(project, ".agents", "skills", "preferred")
	writeSkill(t, projectRoot, "same-name")
	writeSkill(t, filepath.Join(manager.paths.userSkills, "duplicate"), "same-name")
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", "duplicate"), "same-name")

	skills, err := manager.skills(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("skills = %#v, want one preferred skill", skills)
	}
	if skills[0].Name != "same-name" || skills[0].Root != projectRoot {
		t.Fatalf("skill = %#v, want project source at %s", skills[0], projectRoot)
	}
}

func TestDiscoverSkillsIgnoresCodexConfiguration(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", "alpha"), "alpha")
	writeFile(t, filepath.Join(manager.paths.codexHome, "config.toml"), "[[malformed\n")

	skills, err := manager.skills(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "alpha" {
		t.Fatalf("skills = %#v, want alpha discovered independently of Codex config", skills)
	}
}

func TestDiscoverSkillsAllowsLargeInstructionBody(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	content := skillFile("large", "Large skill.", strings.Repeat("x", maxFrontmatterBytes))
	writeFile(t, filepath.Join(manager.paths.userSkills, "large", "SKILL.md"), content)

	skills, err := manager.skills(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "large" {
		t.Fatalf("skills = %#v, want large skill", skills)
	}
}

func TestDiscoverSkillsRejectsEscapingSkillFileSymlink(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	candidate := filepath.Join(manager.paths.userSkills, "escape")
	if err := os.MkdirAll(candidate, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "SKILL.md")
	writeFile(t, external, skillFile("escape", "Escaping skill.", ""))
	if err := os.Symlink(external, filepath.Join(candidate, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	skills, err := manager.skills(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Fatalf("skills = %#v, want escaping SKILL.md symlink rejected", skills)
	}
}

func TestDiscoverSkillsFollowsSkillDirectorySymlink(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	writeSkill(t, target, "linked")
	if err := os.MkdirAll(manager.paths.userSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(manager.paths.userSkills, "linked")); err != nil {
		t.Fatal(err)
	}

	skills, err := manager.skills(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "linked" || skills[0].Root != target {
		t.Fatalf("skills = %#v, want linked skill rooted at %s", skills, target)
	}
}

func writeSkill(t *testing.T, root, name string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "SKILL.md"), skillFile(name, "Skill for "+name+".", ""))
}
