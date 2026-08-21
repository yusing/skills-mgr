package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

const toolingBuiltin = "tooling"

func toolingCallHandler(evidence *projectEvidenceIndex) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != toolingBuiltin {
			return args, nil
		}
		if len(args) != 2 {
			return nil, fmt.Errorf("%s: usage: %s <name>", toolingBuiltin, toolingBuiltin)
		}
		tool, ok := canonicalTool(args[1])
		if !ok {
			return nil, fmt.Errorf("%s: unsupported tool %q", toolingBuiltin, args[1])
		}
		found, err := evidence.has(ctx, evidence.evidence.tooling, tool)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", toolingBuiltin, err)
		}
		return []string{strconv.FormatBool(found)}, nil
	}
}

func canonicalTool(tool string) (string, bool) {
	switch tool {
	case "bun", "yarn", "deno", "npm", "pnpm", "maven", "composer", "cmake", "make",
		"just", "shadowtree", "taskfile", "bazel", "docker", "docker-compose", "kubernetes",
		"pip", "uv":
		return tool, true
	case "k8s":
		return "kubernetes", true
	default:
		return "", false
	}
}

func recordToolingFile(index map[string]bool, name string) {
	switch name {
	case "bun.lock", "bun.lockb", "bunfig.toml":
		index["bun"] = true
	case "yarn.lock", ".yarnrc", ".yarnrc.yml", ".yarnrc.yaml":
		index["yarn"] = true
	case "deno.json", "deno.jsonc", "deno.lock":
		index["deno"] = true
	case "package-lock.json", "npm-shrinkwrap.json":
		index["npm"] = true
	case "pnpm-lock.yaml", "pnpm-workspace.yaml":
		index["pnpm"] = true
	case "pom.xml", "mvnw", "mvnw.cmd":
		index["maven"] = true
	case "composer.json", "composer.lock":
		index["composer"] = true
	case "CMakeLists.txt", "CMakePresets.json", "CMakeUserPresets.json":
		index["cmake"] = true
	case "Makefile", "makefile", "GNUmakefile":
		index["make"] = true
	case "justfile", "Justfile", ".justfile":
		index["just"] = true
	case ".shadowtree.toml":
		index["shadowtree"] = true
	case "Taskfile.yml", "Taskfile.yaml", "taskfile.yml", "taskfile.yaml":
		index["taskfile"] = true
	case ".bazelrc", "MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel", "BUILD", "BUILD.bazel":
		index["bazel"] = true
	case "docker-bake.hcl", "docker-bake.json":
		index["docker"] = true
	case "compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml":
		index["docker"] = true
		index["docker-compose"] = true
	case "kustomization.yml", "kustomization.yaml", "Kustomization", "Chart.yaml", "skaffold.yml", "skaffold.yaml":
		index["kubernetes"] = true
	case "pip.conf", "pip.ini":
		index["pip"] = true
	case "uv.lock", "uv.toml":
		index["uv"] = true
	}
	if name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") {
		index["docker"] = true
	}
	if strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt") {
		index["pip"] = true
	}
}
