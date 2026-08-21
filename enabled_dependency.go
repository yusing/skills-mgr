package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/mod/modfile"
	"mvdan.cc/sh/v3/interp"
)

const dependencyBuiltin = "has_dependency"

var (
	constraintPattern = regexp.MustCompile(`^(>=|==|<=)[[:space:]]*v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?$`)
	lowerBoundPattern = regexp.MustCompile(`(?:^|[[:space:],])([<>]=?|[~^=])?[[:space:]]*v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?`)
)

type dependencyVersion [3]uint64

type dependencyConstraint struct {
	operator  string
	version   dependencyVersion
	precision int
}

type dependencyIndex map[string][]string

type packageManifest struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type cargoManifest struct {
	Dependencies map[string]any `toml:"dependencies"`
	Workspace    struct {
		Dependencies map[string]any `toml:"dependencies"`
	} `toml:"workspace"`
}

func enabledCallHandler(project string) interp.CallHandlerFunc {
	var index dependencyIndex
	var indexErr error
	loaded := false
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != dependencyBuiltin {
			return args, nil
		}
		if len(args) != 3 {
			return nil, fmt.Errorf(
				"%s: usage: %s <name> {>=|==|<=}<version>",
				dependencyBuiltin,
				dependencyBuiltin,
			)
		}
		want, err := parseDependencyConstraint(args[2])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dependencyBuiltin, err)
		}
		if !loaded {
			index, indexErr = loadDependencyIndex(ctx, project)
			loaded = true
		}
		if indexErr != nil {
			return nil, fmt.Errorf("%s: %w", dependencyBuiltin, indexErr)
		}
		return []string{strconv.FormatBool(index.matches(args[1], want))}, nil
	}
}

func loadDependencyIndex(ctx context.Context, project string) (dependencyIndex, error) {
	paths, err := dependencyManifestPaths(ctx, project)
	if err != nil {
		return nil, err
	}
	index := make(dependencyIndex)
	for _, path := range paths {
		if err := index.addManifest(path); err != nil {
			return nil, err
		}
	}
	return index, nil
}

func dependencyManifestPaths(ctx context.Context, project string) ([]string, error) {
	// Git can enumerate tracked and unignored manifests without visiting every file.
	// The directory scan preserves the same behavior for projects outside worktrees.
	output, err := exec.CommandContext(
		ctx,
		"git",
		"-C",
		project,
		"ls-files",
		"-coz",
		"--exclude-standard",
		"--",
		"go.mod",
		"Cargo.toml",
		"package.json",
		":(glob)**/go.mod",
		":(glob)**/Cargo.toml",
		":(glob)**/package.json",
	).Output()
	if err == nil {
		paths := make([]string, 0, bytes.Count(output, []byte{0}))
		for relative := range bytes.SplitSeq(output, []byte{0}) {
			if len(relative) == 0 || !filepath.IsLocal(string(relative)) ||
				ignoredDependencyManifestPath(string(relative)) {
				continue
			}
			paths = append(paths, filepath.Join(project, string(relative)))
		}
		return paths, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return scanDependencyManifestPaths(project)
}

func scanDependencyManifestPaths(project string) ([]string, error) {
	paths := make([]string, 0)
	directories := []string{project}
	for len(directories) > 0 {
		directory := directories[len(directories)-1]
		directories = directories[:len(directories)-1]
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read dependency directory %s: %w", directory, err)
		}
		for _, entry := range entries {
			path := filepath.Join(directory, entry.Name())
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules", "target":
					continue
				}
				directories = append(directories, path)
				continue
			}
			switch entry.Name() {
			case "go.mod", "Cargo.toml", "package.json":
				paths = append(paths, path)
			}
		}
	}
	return paths, nil
}

func ignoredDependencyManifestPath(path string) bool {
	for component := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		switch component {
		case ".git", "node_modules", "target":
			return true
		}
	}
	return false
}

func (index dependencyIndex) addManifest(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read dependency manifest %s: %w", path, err)
	}
	add := func(name, requirement string) {
		if requirement != "" {
			index[name] = append(index[name], requirement)
		}
	}
	switch filepath.Base(path) {
	case "go.mod":
		manifest, err := modfile.ParseLax(path, data, nil)
		if err != nil {
			return fmt.Errorf("decode dependency manifest %s: %w", path, err)
		}
		for _, requirement := range manifest.Require {
			if !requirement.Indirect {
				add(requirement.Mod.Path, requirement.Mod.Version)
			}
		}
		return nil
	case "package.json":
		var manifest packageManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("decode dependency manifest %s: %w", path, err)
		}
		for _, dependencies := range []map[string]string{
			manifest.Dependencies,
			manifest.DevDependencies,
			manifest.OptionalDependencies,
		} {
			for name, requirement := range dependencies {
				add(name, requirement)
			}
		}
		return nil
	}

	var manifest cargoManifest
	if err := toml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode dependency manifest %s: %w", path, err)
	}
	for _, dependencies := range []map[string]any{
		manifest.Dependencies,
		manifest.Workspace.Dependencies,
	} {
		for name, value := range dependencies {
			switch value := value.(type) {
			case string:
				add(name, value)
			case map[string]any:
				version, _ := value["version"].(string)
				add(name, version)
			}
		}
	}
	return nil
}

func (index dependencyIndex) matches(name string, want dependencyConstraint) bool {
	for _, requirement := range index[name] {
		if requirementMatches(requirement, want) {
			return true
		}
	}
	return false
}

func parseDependencyConstraint(constraint string) (dependencyConstraint, error) {
	constraint = strings.TrimSpace(constraint)
	match := constraintPattern.FindStringSubmatch(constraint)
	if match == nil {
		return dependencyConstraint{}, fmt.Errorf(
			"version constraint must use >=, ==, or <= followed by a version",
		)
	}
	version, precision := versionFromMatch(match[2:])
	return dependencyConstraint{
		operator:  match[1],
		version:   version,
		precision: precision,
	}, nil
}

func requirementMatches(requirement string, want dependencyConstraint) bool {
	requirement = strings.TrimSpace(requirement)
	requirement = strings.TrimPrefix(requirement, "workspace:")
	for alternative := range strings.SplitSeq(requirement, "||") {
		matched := false
		for _, match := range lowerBoundPattern.FindAllStringSubmatch(alternative, -1) {
			if strings.HasPrefix(match[1], "<") {
				continue
			}
			matched = true
			version, _ := versionFromMatch(match[2:])
			if !want.matches(version) {
				return false
			}
			break
		}
		if !matched {
			return false
		}
	}
	return requirement != ""
}

func versionFromMatch(parts []string) (dependencyVersion, int) {
	var version dependencyVersion
	precision := 0
	for index, part := range parts {
		if part == "" {
			continue
		}
		version[index], _ = strconv.ParseUint(part, 10, 64)
		precision = index + 1
	}
	return version, precision
}

func (c dependencyConstraint) matches(version dependencyVersion) bool {
	comparison := 0
	for index := range c.precision {
		if version[index] != c.version[index] {
			if version[index] < c.version[index] {
				comparison = -1
			} else {
				comparison = 1
			}
			break
		}
	}
	switch c.operator {
	case ">=":
		return comparison >= 0
	case "==":
		return comparison == 0
	case "<=":
		return comparison <= 0
	default:
		return false
	}
}
