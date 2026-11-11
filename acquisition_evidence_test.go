package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquisitionShortCircuitsSingleEvidenceQuery(t *testing.T) {
	for _, test := range []struct {
		expression string
		file       string
	}{
		{"lang rust", "Cargo.toml"},
		{"lang ts", "main.ts"},
		{"tooling shadowtree", ".shadowtree.toml"},
		{"tooling k8s", "Chart.yaml"},
	} {
		t.Run(test.expression, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			root := filepath.Join(manager.paths.userSkills, "example")
			writeFile(t, filepath.Join(root, "SKILL.md"), skillFile("example", "Example skill.", "body\n"))
			writeExecutable(t, filepath.Join(root, "check.sh"), "#!/bin/sh\nprintf checked\n")
			writeFile(t, filepath.Join(project, test.file), "")
			// A completed scan would fail here. A sufficient root-level match
			// must not visit unrelated descendants at all.
			if err := os.MkdirAll(filepath.Join(project, "unrelated", ".gitignore"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := saveLock(project, testLock(nil, map[string]string{"example": test.expression}, nil)); err != nil {
				t.Fatal(err)
			}

			var output bytes.Buffer
			if err := manager.getContext(t.Context(), project, "example", "", &output); err != nil {
				t.Fatal(err)
			}
			if output.String() != "body\n" {
				t.Fatalf("get output = %q", output.String())
			}
			command, err := manager.scriptCommandContext(t.Context(), project, "example/check.sh", nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := command.Output()
			if err != nil || string(result) != "checked" {
				t.Fatalf("run output = %q, error = %v", result, err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := manager.getContext(ctx, project, "example", "", io.Discard); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled get error = %v", err)
			}
		})
	}
}

func TestAcquisitionKeepsCompleteEvidenceForOtherExpressions(t *testing.T) {
	for _, expression := range []string{
		"lang rust && true",
		`query=lang; "$query" rust`,
		"UNUSED=1 lang rust",
		"lang rust >/dev/null",
		`lang rust >"$(lang rust; printf /dev/null)"`,
		"lang r*",
		"tooling shadow*",
		"has_dependency example",
	} {
		t.Run(expression, func(t *testing.T) {
			manager := newTestManager(t)
			project := t.TempDir()
			writeFile(t, filepath.Join(manager.paths.userSkills, "example", "SKILL.md"), skillFile("example", "Example skill.", "body\n"))
			writeFile(t, filepath.Join(project, "Cargo.toml"), "")
			writeFile(t, filepath.Join(project, "rust"), "")
			writeFile(t, filepath.Join(project, ".shadowtree.toml"), "")
			writeFile(t, filepath.Join(project, "shadowtree"), "")
			if err := os.MkdirAll(filepath.Join(project, "unrelated", ".gitignore"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := saveLock(project, testLock(nil, map[string]string{"example": expression}, nil)); err != nil {
				t.Fatal(err)
			}
			if err := manager.getContext(t.Context(), project, "example", "", io.Discard); err == nil {
				t.Fatal("get succeeded without completing the project snapshot")
			}
		})
	}
}

func TestAcquisitionSingleEvidenceQuerySeesNestedEvidenceAndEdits(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(manager.paths.userSkills, "example", "SKILL.md"), skillFile("example", "Example skill.", "body\n"))
	if err := saveLock(project, testLock(nil, map[string]string{"example": "lang rust"}, nil)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, ".gitignore"), "ignored/\n")
	writeFile(t, filepath.Join(project, "ignored", "Cargo.toml"), "")
	path := filepath.Join(project, "src", "nested", "lib.rs")
	writeFile(t, path, "")
	if err := manager.getContext(t.Context(), project, "example", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := manager.getContext(t.Context(), project, "example", "", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("get after evidence removal = %v, want disabled", err)
	}
	// Absence must exhaust the walk rather than return at the first directory.
	if err := os.MkdirAll(filepath.Join(project, "src", "nested", ".gitignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manager.getContext(t.Context(), project, "example", "", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "read project ignore file") {
		t.Fatalf("absent evidence get error = %v, want traversal error", err)
	}
}
