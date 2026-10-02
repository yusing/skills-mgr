package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistedRemoteRefReadsOnlyRequestedRecord(t *testing.T) {
	manager := newTestManager(t)
	ref, _ := writeLookupRemoteSkill(t, manager, "selected")
	writeFile(t, filepath.Join(manager.paths.remoteSkills, "entries", "unrelated.json"), "{invalid JSON")
	if _, err := manager.remoteStore.records(); err == nil {
		t.Fatal("full catalog accepted malformed unrelated metadata")
	}
	if _, err := manager.remoteStore.recordsForDiscoveryNamed("", ref.Name); err == nil {
		t.Fatal("named discovery skipped validation of unrelated record metadata")
	}
	got, err := manager.persistedRemoteRef(ref.key(), ref.Name)
	if err != nil || got != ref {
		t.Fatalf("persistedRemoteRef = %#v, error = %v; want %#v", got, err, ref)
	}
}

func TestPersistedRemoteRefRejectsInvalidTarget(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-name", "short-key", "non-hex-key", "escaping-key", "malformed-metadata", "unsupported-schema"} {
		t.Run(kind, func(t *testing.T) {
			manager := newTestManager(t)
			ref, record := writeLookupRemoteSkill(t, manager, "selected")
			key, name := ref.key(), ref.Name
			switch kind {
			case "missing":
				key = strings.Repeat("0", 64)
			case "wrong-name":
				name = "other"
			case "short-key":
				key = "abc"
			case "non-hex-key":
				key = strings.Repeat("z", 64)
			case "escaping-key":
				key = "../" + strings.Repeat("a", 61)
			case "malformed-metadata":
				writeFile(t, filepath.Join(manager.paths.remoteSkills, "entries", key+".json"), "{invalid JSON")
			case "unsupported-schema":
				record.SchemaRevision = remoteSkillSchemaRevision + 1
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(manager.paths.remoteSkills, "entries", key+".json"), string(data))
			}
			if got, err := manager.persistedRemoteRef(key, name); err == nil {
				t.Fatalf("invalid %s target returned %#v without an error", kind, got)
			}
		})
	}
}

func TestGetNamedRemoteDiscoverySkipsUnrelatedContentAndOverrides(t *testing.T) {
	for _, kind := range []string{"missing-content", "invalid-override"} {
		t.Run(kind, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			selected, _ := writeLookupRemoteSkill(t, manager, "selected")
			unrelated, record := writeLookupRemoteSkill(t, manager, "unrelated")
			if err := saveLock(project, testLock(map[string]bool{"selected": true}, nil, map[string]remoteSkillRef{"selected": selected})); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing-content":
				root, err := manager.remoteStore.contentRoot(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(root, filepath.Join(t.TempDir(), "removed-content")); err != nil {
					t.Fatal(err)
				}
			case "invalid-override":
				writeFile(t, manager.remoteStore.overridePath(unrelated), "{invalid JSON")
			}
			if _, err := manager.discoverSkills(project, ""); (err != nil) != (kind == "invalid-override") {
				t.Fatalf("full discovery of unrelated %s: %v", kind, err)
			}
			records, err := manager.remoteStore.recordsForDiscoveryNamed("", "selected")
			if err != nil || len(records) != 1 || records[0].ref() != selected {
				t.Fatalf("named records = %#v, error = %v; want selected record", records, err)
			}
			excluded, err := manager.remoteStore.recordsForDiscoveryNamed(selected.key(), "selected")
			if err != nil || len(excluded) != 0 {
				t.Fatalf("excluded named records = %#v, error = %v", excluded, err)
			}
			var output bytes.Buffer
			if err := manager.getContext(t.Context(), project, "selected", "", &output); err != nil {
				t.Fatal(err)
			}
			if output.String() != "selected body\n" {
				t.Fatalf("get output = %q, want selected body", output.String())
			}
		})
	}
}

func writeLookupRemoteSkill(t *testing.T, manager *manager, name string) (remoteSkillRef, remoteSkillRecord) {
	t.Helper()
	ref := remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/" + name,
		Name:     name,
		Locator:  "owner/repo/" + name,
	}
	record, err := manager.remoteStore.ensure(t.Context(), ref, &staticRemoteProvider{files: []remoteSkillFile{{
		Path:     "SKILL.md",
		Contents: []byte(skillFile(name, "Remote skill.", name+" body\n")),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return ref, record
}
