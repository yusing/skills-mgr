package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnabledHasDependencyBuiltin(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		manifest   string
		expression string
		want       bool
	}{
		{
			name:       "nested cargo string version",
			path:       "apps/desktop/native/Cargo.toml",
			manifest:   "[dependencies]\ntauri = \"2.8.5\"\n",
			expression: "has_dependency tauri '>=2'",
			want:       true,
		},
		{
			name:       "cargo inline version",
			path:       "Cargo.toml",
			manifest:   "[workspace.dependencies]\ntauri = { version = \"3\", features = [] }\n",
			expression: "has_dependency tauri '>=2'",
			want:       true,
		},
		{
			name:       "cargo development dependency",
			path:       "Cargo.toml",
			manifest:   "[dev-dependencies]\ntauri = \"2.8.5\"\n",
			expression: "has_dependency tauri '>=2'",
			want:       true,
		},
		{
			name:       "cargo build dependency",
			path:       "Cargo.toml",
			manifest:   "[build-dependencies]\ntauri = \"2.8.5\"\n",
			expression: "has_dependency tauri '>=2'",
			want:       true,
		},
		{
			name:       "cargo target dependency",
			path:       "Cargo.toml",
			manifest:   "[target.'cfg(unix)'.dependencies]\ntauri = \"2.8.5\"\n",
			expression: "has_dependency tauri '>=2'",
			want:       true,
		},
		{
			name:       "cargo path dependency without constraint",
			path:       "Cargo.toml",
			manifest:   "[dependencies]\nlocal-lib = { path = \"../local-lib\" }\n",
			expression: "has_dependency local-lib",
			want:       true,
		},
		{
			name:       "cargo path dependency with constraint",
			path:       "Cargo.toml",
			manifest:   "[dependencies]\nlocal-lib = { path = \"../local-lib\" }\n",
			expression: "has_dependency local-lib '>=1'",
		},
		{
			name:       "cargo version too old",
			path:       "native/Cargo.toml",
			manifest:   "[dependencies]\ntauri = \"1.6\"\n",
			expression: "has_dependency tauri '>=2'",
		},
		{
			name:       "package dependency",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"^2.2.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '>=2'",
			want:       true,
		},
		{
			name:       "package dependency without constraint",
			path:       "package.json",
			manifest:   `{"dependencies":{"react":"^19.1.0"}}`,
			expression: "has_dependency react",
			want:       true,
		},
		{
			name:       "missing package without constraint",
			path:       "package.json",
			manifest:   `{"dependencies":{"react":"^19.1.0"}}`,
			expression: "has_dependency vue",
		},
		{
			name:       "package dev dependency too old",
			path:       "app/package.json",
			manifest:   `{"devDependencies":{"@tauri-apps/cli":"~1.5.0"}}`,
			expression: "has_dependency '@tauri-apps/cli' '>=2'",
		},
		{
			name:       "generated dependency tree ignored",
			path:       "node_modules/example/package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"2"}}`,
			expression: "has_dependency '@tauri-apps/api' '>=2'",
		},
		{
			name:       "alternative allows old version",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"^1 || ^2"}}`,
			expression: "has_dependency '@tauri-apps/api' '>=2'",
		},
		{
			name:       "package major equality",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"^2.2.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '==2'",
			want:       true,
		},
		{
			name:       "package minor upper bound",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"^2.2.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '<=2.1'",
		},
		{
			name:       "package exclusive major upper bound",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"^2.2.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '<3'",
			want:       true,
		},
		{
			name:       "package reaches exclusive upper bound",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"3.0.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '<3'",
		},
		{
			name:       "package constraint conjunction",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"2.8.5"}}`,
			expression: "has_dependency '@tauri-apps/api' '>=2 && <3'",
			want:       true,
		},
		{
			name:       "package constraint conjunction rejects upper bound",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"3.0.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '>=2 && <3'",
		},
		{
			name:       "package constraint disjunction",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"3.0.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '==2 || ==3'",
			want:       true,
		},
		{
			name:       "package constraint disjunction rejects alternatives",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"3.0.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '==1 || ==2'",
		},
		{
			name:       "package constraint operator precedence",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":"3.0.0"}}`,
			expression: "has_dependency '@tauri-apps/api' '<2 || >=3 && <4'",
			want:       true,
		},
		{
			name:       "package range crosses exclusive upper bound",
			path:       "package.json",
			manifest:   `{"dependencies":{"@tauri-apps/api":">=2 <4"}}`,
			expression: "has_dependency '@tauri-apps/api' '<3'",
		},
		{
			name:       "go direct dependency",
			path:       "go.mod",
			manifest:   "module example.com/app\n\ngo 1.26\n\nrequire example.com/framework/v2 v2.3.1\n",
			expression: "has_dependency example.com/framework/v2 '==2'",
			want:       true,
		},
		{
			name:       "go direct dependency without constraint",
			path:       "go.mod",
			manifest:   "module example.com/app\n\ngo 1.26\n\nrequire example.com/framework/v2 v2.3.1\n",
			expression: "has_dependency example.com/framework/v2",
			want:       true,
		},
		{
			name:       "go indirect dependency",
			path:       "go.mod",
			manifest:   "module example.com/app\n\ngo 1.26\n\nrequire example.com/framework/v2 v2.3.1 // indirect\n",
			expression: "has_dependency example.com/framework/v2 '>=2'",
		},
		{
			name:       "go indirect dependency without constraint",
			path:       "go.mod",
			manifest:   "module example.com/app\n\ngo 1.26\n\nrequire example.com/framework/v2 v2.3.1 // indirect\n",
			expression: "has_dependency example.com/framework/v2",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project+"/"+test.path, test.manifest)
			got, err := evaluateEnabled(t.Context(), project, "tauri-v2", test.expression)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("has_dependency = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEnabledHasDependencyBuiltinComposesAndValidatesArguments(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/package.json", `{"devDependencies":{"@tauri-apps/cli":"2"}}`)
	enabled, err := evaluateEnabled(
		t.Context(),
		project,
		"tauri-v2",
		"has_dependency tauri '>=2' || has_dependency '@tauri-apps/cli' '>=2'",
	)
	if err != nil || !enabled {
		t.Fatalf("composed dependency expression = %v, %v", enabled, err)
	}

	_, err = evaluateEnabled(t.Context(), project, "tauri-v2", "has_dependency")
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("invalid builtin arguments error = %v", err)
	}
	_, err = evaluateEnabled(
		t.Context(),
		project,
		"tauri-v2",
		"has_dependency tauri '>=2' extra",
	)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("extra builtin arguments error = %v", err)
	}
	_, err = evaluateEnabled(
		t.Context(),
		project,
		"tauri-v2",
		"has_dependency '@tauri-apps/cli' '^2'",
	)
	if err == nil || !strings.Contains(err.Error(), ">=, ==, <=, or <") {
		t.Fatalf("invalid builtin constraint error = %v", err)
	}
	_, err = evaluateEnabled(
		t.Context(),
		project,
		"tauri-v2",
		"has_dependency '@tauri-apps/cli' '>=2 &&'",
	)
	if err == nil || !strings.Contains(err.Error(), ">=, ==, <=, or <") {
		t.Fatalf("incomplete builtin constraint error = %v", err)
	}
}

func TestEnabledHasDependencyBuiltinUsesOneManifestSnapshot(t *testing.T) {
	project := t.TempDir()
	path := project + "/package.json"
	writeFile(t, path, `{"dependencies":{}}`)
	handler := enabledCallHandler(project)
	args, err := handler(
		t.Context(),
		[]string{dependencyBuiltin, "@tauri-apps/api", ">=2"},
	)
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("first dependency result = %v, %v", args, err)
	}
	writeFile(t, path, `{"dependencies":{"@tauri-apps/api":"2"}}`)
	args, err = handler(
		t.Context(),
		[]string{dependencyBuiltin, "@tauri-apps/api", ">=2"},
	)
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("cached dependency result = %v, %v", args, err)
	}
}

func TestEnabledHasDependencyBuiltinRequiresOneMatchingDeclaration(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/v1/package.json", `{"dependencies":{"framework":"1.9.0"}}`)
	writeFile(t, project+"/v3/package.json", `{"dependencies":{"framework":"3.1.0"}}`)
	enabled, err := evaluateEnabled(
		t.Context(),
		project,
		"framework-v2",
		"has_dependency framework '>=2 && <3'",
	)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("conjunction matched different dependency declarations")
	}
}

func TestScanDependencyManifestPathsStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := scanDependencyManifestPaths(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", err)
	}
}

func TestEnabledHasDependencyBuiltinLoadsManifestConcurrently(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/package.json", `{"dependencies":{"@tauri-apps/api":"2"}}`)
	enabled, err := evaluateEnabled(
		t.Context(),
		project,
		"tauri-v2",
		"has_dependency '@tauri-apps/api' '>=2' & has_dependency '@tauri-apps/api' '<3' & wait",
	)
	if err != nil || !enabled {
		t.Fatalf("concurrent dependency expression = %v, %v", enabled, err)
	}
}
