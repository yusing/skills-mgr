package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestEnabledLangBuiltinDetectsSupportedLanguages(t *testing.T) {
	for _, test := range []struct {
		language string
		path     string
	}{
		{"go", "go.mod"},
		{"rust", "Cargo.toml"},
		{"node", "package.json"},
		{"typescript", "src/main.ts"},
		{"tsx", "src/component.tsx"},
		{"javascript", "src/main.mjs"},
		{"jsx", "src/component.jsx"},
		{"html", "public/index.html"},
		{"css", "public/app.css"},
		{"python", "pyproject.toml"},
		{"c", "src/main.c"},
		{"c++", "src/main.cpp"},
		{"c#", "src/Program.cs"},
		{"java", "src/Main.java"},
		{"lua", "init.lua"},
		{"vb", "src/App.vb"},
		{"php", "composer.json"},
		{"r", "analysis.R"},
		{"ruby", "Gemfile"},
		{"swift", "Package.swift"},
		{"perl", "cpanfile"},
		{"assembly", "src/start.S"},
		{"shell", "scripts/check.sh"},
		{"bash", "scripts/check.bash"},
		{"postgres", "schema.psql"},
		{"sql", "schema.sql"},
		{"yaml", "config.yml"},
		{"json", "config.json"},
		{"toml", "config.toml"},
		{"ini", "config.ini"},
	} {
		t.Run(test.language, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project+"/"+test.path, "")
			enabled, err := evaluateEnabled(t.Context(), project, "example", "lang '"+test.language+"'")
			if err != nil || !enabled {
				t.Fatalf("lang %s = %v, %v", test.language, enabled, err)
			}
		})
	}
}

func TestEnabledLangBuiltinAliasesAndMixedProjects(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/go.mod", "module example.com/app\n")
	writeFile(t, project+"/src/app.tsx", "")
	writeFile(t, project+"/src/app.jsx", "")
	writeFile(t, project+"/src/start.asm", "")
	writeFile(t, project+"/src/main.cpp", "")
	writeFile(t, project+"/src/Program.cs", "")
	writeFile(t, project+"/scripts/check.sh", "")
	writeFile(t, project+"/public/index.html", "")
	writeFile(t, project+"/public/app.css", "")

	for _, expression := range []string{
		"lang ts",
		"lang js",
		"lang tsx",
		"lang jsx",
		"lang html",
		"lang css",
		"lang asm",
		"lang sh",
		"lang c++",
		"lang c#",
		"lang go && lang typescript && lang javascript && lang assembly && lang shell",
	} {
		enabled, err := evaluateEnabled(t.Context(), project, "example", expression)
		if err != nil || !enabled {
			t.Fatalf("%q = %v, %v", expression, enabled, err)
		}
	}

	enabled, err := evaluateEnabled(t.Context(), project, "example", "lang rust")
	if err != nil || enabled {
		t.Fatalf("absent language = %v, %v", enabled, err)
	}
}

func TestEnabledLangBuiltinComposesAndValidatesArguments(t *testing.T) {
	project := t.TempDir()
	writeFile(
		t,
		project+"/package.json",
		`{"dependencies":{"@tauri-apps/api":"2"}}`,
	)
	enabled, err := evaluateEnabled(
		t.Context(),
		project,
		"example",
		"lang node && has_dependency '@tauri-apps/api' '==2'",
	)
	if err != nil || !enabled {
		t.Fatalf("composed builtin expression = %v, %v", enabled, err)
	}
	enabled, err = evaluateEnabled(
		t.Context(),
		project,
		"example",
		"lang node && sh -c 'exit 0'",
	)
	if err != nil || !enabled {
		t.Fatalf("external command composition = %v, %v", enabled, err)
	}

	for _, expression := range []string{"lang", "lang go rust"} {
		_, err := evaluateEnabled(t.Context(), project, "example", expression)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%q error = %v", expression, err)
		}
	}
	_, err = evaluateEnabled(t.Context(), project, "example", "lang kotlin")
	if err == nil || !strings.Contains(err.Error(), `unsupported language "kotlin"`) {
		t.Fatalf("unsupported language error = %v", err)
	}
}

func TestEnabledLangBuiltinIncludesGitIgnoredEvidence(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/.gitignore", "generated/\n")
	writeFile(t, project+"/generated/app.ts", "")

	enabled, err := evaluateEnabled(t.Context(), project, "example", "lang ts")
	if err != nil || !enabled {
		t.Fatalf("Git-ignored language expression = %v, %v", enabled, err)
	}
}

func TestEnabledLangBuiltinUsesOnlyDirectEvidenceAtHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, home+"/projects/example/go.mod", "module example.com/app\n")

	enabled, err := evaluateEnabled(t.Context(), home, "example", "lang go")
	if err != nil || enabled {
		t.Fatalf("nested home language expression = %v, %v; want false", enabled, err)
	}

	writeFile(t, home+"/go.mod", "module example.com/home\n")
	enabled, err = evaluateEnabled(t.Context(), home, "example", "lang go")
	if err != nil || !enabled {
		t.Fatalf("direct home language expression = %v, %v; want true", enabled, err)
	}
}

func TestEnabledLangBuiltinHonorsCanceledContextDuringEvidenceLoad(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := evaluateEnabled(ctx, t.TempDir(), "example", "lang go")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled language expression error = %v, want context.Canceled", err)
	}
}

func TestEnabledLangBuiltinCachesVisitedEvidence(t *testing.T) {
	project := t.TempDir()
	path := project + "/app.ts"
	writeFile(t, path, "")
	handler := enabledCallHandler(project)
	args, err := handler(t.Context(), []string{languageBuiltin, "typescript"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("initial language result = %v, %v", args, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	args, err = handler(t.Context(), []string{languageBuiltin, "typescript"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("cached language result = %v, %v", args, err)
	}
}

func TestEnabledLangBuiltinIgnoresDependencyAndGitDirectories(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/node_modules/example/index.ts", "")
	writeFile(t, project+"/target/generated/lib.rs", "")
	writeFile(t, project+"/.git/hooks/check.py", "")
	for _, language := range []string{"typescript", "rust", "python"} {
		enabled, err := evaluateEnabled(t.Context(), project, "example", "lang "+language)
		if err != nil || enabled {
			t.Fatalf("ignored lang %s = %v, %v", language, enabled, err)
		}
	}
}

func TestEnabledLangBuiltinUsesOneProjectSnapshot(t *testing.T) {
	project := t.TempDir()
	handler := enabledCallHandler(project)
	args, err := handler(t.Context(), []string{languageBuiltin, "go"})
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("first language result = %v, %v", args, err)
	}
	writeFile(t, project+"/go.mod", "module example.com/app\n")
	args, err = handler(t.Context(), []string{languageBuiltin, "go"})
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("cached language result = %v, %v", args, err)
	}
}

func TestEnabledLangBuiltinKeepsOneCompletedProjectSnapshot(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/a/.keep", "")
	writeFile(t, project+"/z/main.go", "package main\n")
	handler := enabledCallHandler(project)
	args, err := handler(t.Context(), []string{languageBuiltin, "go"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("first language result = %v, %v", args, err)
	}

	writeFile(t, project+"/a/lib.rs", "")
	args, err = handler(t.Context(), []string{languageBuiltin, "rust"})
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("cached language result = %v, %v", args, err)
	}

	fresh := enabledCallHandler(project)
	args, err = fresh(t.Context(), []string{languageBuiltin, "rust"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("fresh language result = %v, %v", args, err)
	}
}

func TestEnabledLangBuiltinLoadsOneSnapshotConcurrently(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/go.mod", "module example.com/app\n")
	expression := strings.Repeat("lang go & ", 32) + "wait"
	enabled, err := evaluateEnabled(t.Context(), project, "example", expression)
	if err != nil || !enabled {
		t.Fatalf("concurrent language expression = %v, %v", enabled, err)
	}
}
