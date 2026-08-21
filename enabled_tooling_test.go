package main

import (
	"strings"
	"testing"
)

func TestEnabledToolingBuiltinDetectsSupportedTools(t *testing.T) {
	for _, test := range []struct {
		tool string
		path string
	}{
		{"bun", "bun.lock"},
		{"yarn", "yarn.lock"},
		{"deno", "deno.json"},
		{"npm", "package-lock.json"},
		{"pnpm", "pnpm-lock.yaml"},
		{"maven", "pom.xml"},
		{"composer", "composer.lock"},
		{"cmake", "CMakeLists.txt"},
		{"make", "Makefile"},
		{"just", "justfile"},
		{"shadowtree", ".shadowtree.toml"},
		{"taskfile", "Taskfile.yaml"},
		{"bazel", "MODULE.bazel"},
		{"docker", "docker/Dockerfile.dev"},
		{"docker-compose", "compose.yaml"},
		{"kubernetes", "deploy/kustomization.yaml"},
		{"pip", "requirements-dev.txt"},
		{"uv", "uv.lock"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project+"/"+test.path, "")
			enabled, err := evaluateEnabled(t.Context(), project, "example", "tooling "+test.tool)
			if err != nil || !enabled {
				t.Fatalf("tooling %s = %v, %v", test.tool, enabled, err)
			}
		})
	}
}

func TestEnabledToolingBuiltinAliasesAndMixedProjects(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/compose.yaml", "")
	writeFile(t, project+"/deploy/Chart.yaml", "")
	writeFile(t, project+"/.shadowtree.toml", "")

	for _, expression := range []string{
		"tooling k8s",
		"tooling kubernetes",
		"tooling docker-compose && tooling docker && lang yaml",
		"tooling shadowtree && tooling kubernetes",
	} {
		enabled, err := evaluateEnabled(t.Context(), project, "example", expression)
		if err != nil || !enabled {
			t.Fatalf("%q = %v, %v", expression, enabled, err)
		}
	}

	enabled, err := evaluateEnabled(t.Context(), project, "example", "tooling npm")
	if err != nil || enabled {
		t.Fatalf("absent tooling = %v, %v", enabled, err)
	}
}

func TestEnabledToolingBuiltinComposesAndValidatesArguments(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/package-lock.json", "{}")
	writeFile(t, project+"/package.json", `{"dependencies":{"react":"19"}}`)
	enabled, err := evaluateEnabled(
		t.Context(),
		project,
		"example",
		"tooling npm && lang json && has_dependency react",
	)
	if err != nil || !enabled {
		t.Fatalf("composed builtin expression = %v, %v", enabled, err)
	}
	enabled, err = evaluateEnabled(
		t.Context(),
		project,
		"example",
		"tooling npm && sh -c 'exit 0'",
	)
	if err != nil || !enabled {
		t.Fatalf("external command composition = %v, %v", enabled, err)
	}

	for _, expression := range []string{"tooling", "tooling npm yarn"} {
		_, err := evaluateEnabled(t.Context(), project, "example", expression)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%q error = %v", expression, err)
		}
	}
	_, err = evaluateEnabled(t.Context(), project, "example", "tooling gradle")
	if err == nil || !strings.Contains(err.Error(), `unsupported tool "gradle"`) {
		t.Fatalf("unsupported tool error = %v", err)
	}
}

func TestEnabledToolingBuiltinIgnoresDependencyAndGitDirectories(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/node_modules/example/yarn.lock", "")
	writeFile(t, project+"/target/generated/CMakeLists.txt", "")
	writeFile(t, project+"/.git/hooks/Taskfile.yml", "")
	for _, tool := range []string{"yarn", "cmake", "taskfile"} {
		enabled, err := evaluateEnabled(t.Context(), project, "example", "tooling "+tool)
		if err != nil || enabled {
			t.Fatalf("ignored tooling %s = %v, %v", tool, enabled, err)
		}
	}
}

func TestEnabledToolingBuiltinCachesExhaustedProjectWalk(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/go.mod", "module example.com/app\n")
	handler := enabledCallHandler(project)
	args, err := handler(t.Context(), []string{toolingBuiltin, "npm"})
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("first tooling result = %v, %v", args, err)
	}
	writeFile(t, project+"/package-lock.json", "{}")
	writeFile(t, project+"/main.ts", "")
	for _, call := range [][]string{
		{toolingBuiltin, "npm"},
		{languageBuiltin, "typescript"},
	} {
		args, err = handler(t.Context(), call)
		if err != nil || len(args) != 1 || args[0] != "false" {
			t.Fatalf("cached result for %v = %v, %v", call, args, err)
		}
	}
	args, err = handler(t.Context(), []string{languageBuiltin, "go"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("shared language result = %v, %v", args, err)
	}
}

func TestEnabledToolingBuiltinKeepsOneCompletedProjectSnapshot(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/a/.keep", "")
	writeFile(t, project+"/z/main.go", "package main\n")
	handler := enabledCallHandler(project)
	args, err := handler(t.Context(), []string{languageBuiltin, "go"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("first language result = %v, %v", args, err)
	}

	writeFile(t, project+"/a/package-lock.json", "{}")
	args, err = handler(t.Context(), []string{toolingBuiltin, "npm"})
	if err != nil || len(args) != 1 || args[0] != "false" {
		t.Fatalf("cached tooling result = %v, %v", args, err)
	}

	fresh := enabledCallHandler(project)
	args, err = fresh(t.Context(), []string{toolingBuiltin, "npm"})
	if err != nil || len(args) != 1 || args[0] != "true" {
		t.Fatalf("fresh tooling result = %v, %v", args, err)
	}
}

func TestEnabledToolingBuiltinLoadsOneSnapshotConcurrently(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project+"/package-lock.json", "{}")
	expression := strings.Repeat("tooling npm & ", 32) + "wait"
	enabled, err := evaluateEnabled(t.Context(), project, "example", expression)
	if err != nil || !enabled {
		t.Fatalf("concurrent tooling expression = %v, %v", enabled, err)
	}
}
