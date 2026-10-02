package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverSkillsNamedMatchesRootPrecedence(t *testing.T) {
	// Each suffix leaves a different root as the highest-precedence owner.
	for first, name := range []string{"project", "user", "managed", "project-claude", "project-grok", "project-codex", "bundled", "codex", "admin", "plugin"} {
		t.Run(name, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			roots := []struct {
				path, source string
			}{
				{filepath.Join(project, ".agents", "skills", "entry"), "project"},
				{filepath.Join(manager.paths.userSkills, "entry"), "user"},
				{filepath.Join(manager.paths.managedSkills, "entry"), "managed"},
				{filepath.Join(project, ".claude", "skills", "entry"), "claude"},
				{filepath.Join(project, ".grok", "skills", "entry"), "grok"},
				{filepath.Join(project, ".codex", "skills", "entry"), "codex"},
				{filepath.Join(manager.paths.codexHome, "skills", ".system", "entry"), "bundled"},
				{filepath.Join(manager.paths.codexHome, "skills", "entry"), "codex"},
				{filepath.Join(manager.paths.adminSkills, "entry"), "admin"},
				{filepath.Join(manager.paths.codexPluginCache(), "publisher", "plugin", "hash", "skills", "entry"), "plugin"},
			}
			for _, root := range roots[first:] {
				writeSkill(t, root.path, "shared")
			}
			assertNamedDiscoveryOwner(t, manager, project, "shared", roots[first].path, roots[first].source)
		})
	}
}

func TestDiscoverSkillsNamedMatchesEntryOrdering(t *testing.T) {
	for _, kind := range []string{"direct", "system", "plugin"} {
		t.Run(kind, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			var first, later, source string
			switch kind {
			case "direct":
				first = filepath.Join(manager.paths.userSkills, "a-different-directory")
				later = filepath.Join(manager.paths.userSkills, "shared")
				source = "user"
				writeSkill(t, filepath.Join(manager.paths.userSkills, "0-unrelated"), "unrelated")
			case "system":
				first = filepath.Join(manager.paths.codexHome, "skills", ".system", "a-different-directory")
				later = filepath.Join(manager.paths.codexHome, "skills", "shared")
				source = "bundled"
				writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", ".system", "0-unrelated"), "unrelated")
			case "plugin":
				first = filepath.Join(manager.paths.codexPluginCache(), "a-publisher", "plugin", "hash", "skills", "a-different-directory")
				later = filepath.Join(manager.paths.codexPluginCache(), "z-publisher", "plugin", "hash", "skills", "shared")
				source = "plugin"
				writeSkill(t, filepath.Join(filepath.Dir(first), "0-unrelated"), "unrelated")
				writeSkill(t, filepath.Join(filepath.Dir(first), "shared"), "shared")
			}
			writeSkill(t, first, "shared")
			writeSkill(t, later, "shared")
			assertNamedDiscoveryOwner(t, manager, project, "shared", first, source)
		})
	}
}

func TestDiscoverSkillsNamedMatchesSymlinkAndPlaceholderFiltering(t *testing.T) {
	for _, kind := range []string{"directory-symlink", "manifest-symlink", "escaping-manifest", "placeholder"} {
		t.Run(kind, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			first := filepath.Join(manager.paths.userSkills, "a-entry")
			later := filepath.Join(manager.paths.userSkills, "z-entry")
			writeSkill(t, later, "shared")
			wantRoot := first
			switch kind {
			case "directory-symlink":
				wantRoot = filepath.Join(t.TempDir(), "external-skill")
				writeSkill(t, wantRoot, "shared")
				if err := os.Symlink(wantRoot, first); err != nil {
					t.Fatal(err)
				}
			case "manifest-symlink", "escaping-manifest":
				manifest := filepath.Join(first, "metadata.md")
				if kind == "escaping-manifest" {
					manifest = filepath.Join(t.TempDir(), "metadata.md")
					wantRoot = later
				}
				writeFile(t, manifest, skillFile("shared", "Linked metadata.", ""))
				if err := os.MkdirAll(first, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(manifest, filepath.Join(first, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "placeholder":
				writeSkill(t, first, "shared")
				writeFile(t, filepath.Join(first, remotePlaceholderMarkerName), remotePlaceholderMarker)
				wantRoot = later
			}
			assertNamedDiscoveryOwner(t, manager, project, "shared", wantRoot, "user")
			missing, err := manager.discoverSkillsNamed(project, "", "absent")
			if err != nil || len(missing) != 0 {
				t.Fatalf("absent lookup = %#v, error = %v", missing, err)
			}
		})
	}
}

func assertNamedDiscoveryOwner(t *testing.T, manager *manager, project, name, root, source string) {
	t.Helper()
	all, err := manager.discoverSkills(project, "")
	if err != nil {
		t.Fatal(err)
	}
	var owner discoveredSkill
	for _, skill := range all {
		if skill.Name == name {
			owner = skill
			break
		}
	}
	if owner.Root != resolvedPath(t, root) || owner.Source != source {
		t.Fatalf("full discovery owner = %#v, want root %s and source %s", owner, root, source)
	}
	named, err := manager.discoverSkillsNamed(project, "", name)
	if err != nil {
		t.Fatal(err)
	}
	if len(named) != 1 || !reflect.DeepEqual(named[0], owner) {
		t.Fatalf("named discovery = %#v, want full discovery owner %#v", named, owner)
	}
}

func TestGetNamedDiscoverySeesEditsAndSelectionChanges(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	manifest := filepath.Join(manager.paths.userSkills, "different-directory", "SKILL.md")
	writeFile(t, manifest, skillFile("shared", "Original description.", "original body\n"))
	setEnabled := func(enabled bool) {
		t.Helper()
		if err := saveLock(project, testLock(map[string]bool{"shared": enabled}, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	get := func(want string) {
		t.Helper()
		var output bytes.Buffer
		if err := manager.getContext(t.Context(), project, "shared", "", &output); err != nil {
			t.Fatal(err)
		}
		if output.String() != want {
			t.Fatalf("get output = %q, want %q", output.String(), want)
		}
	}
	setEnabled(true)
	get("original body\n")
	writeFile(t, manifest, "---\nname: shared\ndescription: Edited description.\ndisable-model-invocation: true\n---\nedited body\n")
	get("---\nname: shared\ndescription: Edited description.\n---\nedited body\n")
	setEnabled(false)
	var output bytes.Buffer
	if err := manager.getContext(t.Context(), project, "shared", "", &output); err == nil {
		t.Fatal("get reused an enabled selection after it was disabled")
	}
	if output.Len() != 0 {
		t.Fatalf("disabled get wrote output: %q", output.String())
	}
	setEnabled(true)
	get("---\nname: shared\ndescription: Edited description.\n---\nedited body\n")
	writeFile(t, manifest, skillFile("shared", "Final description.", "final body\n"))
	get("final body\n")
}

func TestGetNamedDiscoverySkipsUnrelatedInvalidRemoteCatalog(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "entry", "SKILL.md"), skillFile("local", "Local skill.", "local body\n"))
	if err := saveLock(project, testLock(map[string]bool{"local": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(manager.paths.remoteSkills, "entries", "invalid.json"), "{invalid JSON")
	if _, err := manager.discoverSkills(project, ""); err == nil {
		t.Fatal("full discovery unexpectedly accepted the invalid remote catalog")
	}
	var output bytes.Buffer
	if err := manager.getContext(t.Context(), project, "local", "", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "local body\n" {
		t.Fatalf("get output = %q, want local body", output.String())
	}
}
