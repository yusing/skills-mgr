package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/mod/modfile"
	"mvdan.cc/sh/v3/interp"
)

const dependencyBuiltin = "has_dependency"

var (
	constraintPattern = regexp.MustCompile(`^(>=|==|<=|<)[[:space:]]*v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?$`)
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
	Dependencies      map[string]any `toml:"dependencies"`
	DevDependencies   map[string]any `toml:"dev-dependencies"`
	BuildDependencies map[string]any `toml:"build-dependencies"`
	Target            map[string]struct {
		Dependencies      map[string]any `toml:"dependencies"`
		DevDependencies   map[string]any `toml:"dev-dependencies"`
		BuildDependencies map[string]any `toml:"build-dependencies"`
	} `toml:"target"`
	Workspace struct {
		Dependencies map[string]any `toml:"dependencies"`
	} `toml:"workspace"`
}

func dependencyCallHandler(evidence *projectEvidenceIndex) interp.CallHandlerFunc {
	var index dependencyIndex
	var indexErr error
	var loadOnce sync.Once
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != dependencyBuiltin {
			return args, nil
		}
		if len(args) < 2 || len(args) > 3 {
			return nil, fmt.Errorf(
				"%s: usage: %s <name> [<constraint-expression>]",
				dependencyBuiltin,
				dependencyBuiltin,
			)
		}
		var constraintGroups [][]dependencyConstraint
		if len(args) == 3 {
			var err error
			constraintGroups, err = parseDependencyConstraintExpression(args[2])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", dependencyBuiltin, err)
			}
		}
		loadOnce.Do(func() {
			index, indexErr = loadDependencyIndex(ctx, evidence)
		})
		if indexErr != nil {
			return nil, fmt.Errorf("%s: %w", dependencyBuiltin, indexErr)
		}
		if len(args) == 2 {
			_, found := index[args[1]]
			return []string{strconv.FormatBool(found)}, nil
		}
		for _, constraints := range constraintGroups {
			if index.matchesAll(args[1], constraints) {
				return []string{"true"}, nil
			}
		}
		return []string{"false"}, nil
	}
}

func loadDependencyIndex(
	ctx context.Context,
	evidence *projectEvidenceIndex,
) (dependencyIndex, error) {
	paths, err := evidence.manifestPaths(ctx)
	if err != nil {
		return nil, err
	}
	index := make(dependencyIndex)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := index.addManifest(path); err != nil {
			return nil, err
		}
	}
	return index, nil
}

func (index dependencyIndex) addManifest(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read dependency manifest %s: %w", path, err)
	}
	add := func(name, requirement string) {
		// An empty requirement records a versionless Cargo path or Git dependency.
		index[name] = append(index[name], requirement)
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
	addCargoDependencies := func(dependencies map[string]any) {
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
	for _, dependencies := range []map[string]any{
		manifest.Dependencies,
		manifest.DevDependencies,
		manifest.BuildDependencies,
		manifest.Workspace.Dependencies,
	} {
		addCargoDependencies(dependencies)
	}
	for _, target := range manifest.Target {
		for _, dependencies := range []map[string]any{
			target.Dependencies,
			target.DevDependencies,
			target.BuildDependencies,
		} {
			addCargoDependencies(dependencies)
		}
	}
	return nil
}

func (index dependencyIndex) matchesAll(name string, wants []dependencyConstraint) bool {
	for _, requirement := range index[name] {
		matched := true
		for _, want := range wants {
			if !requirementMatches(requirement, want) {
				matched = false
				break
			}
		}
		if matched {
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
			"version constraint must use >=, ==, <=, or < followed by a version",
		)
	}
	version, precision := versionFromMatch(match[2:])
	return dependencyConstraint{
		operator:  match[1],
		version:   version,
		precision: precision,
	}, nil
}

func parseDependencyConstraintExpression(expression string) ([][]dependencyConstraint, error) {
	// The outer slice contains OR alternatives; each inner slice is an AND group.
	groups := make([][]dependencyConstraint, 0)
	for alternative := range strings.SplitSeq(expression, "||") {
		constraints := make([]dependencyConstraint, 0)
		for part := range strings.SplitSeq(alternative, "&&") {
			constraint, err := parseDependencyConstraint(part)
			if err != nil {
				return nil, fmt.Errorf("invalid constraint %q: %w", strings.TrimSpace(part), err)
			}
			constraints = append(constraints, constraint)
		}
		groups = append(groups, constraints)
	}
	return groups, nil
}

func requirementMatches(requirement string, want dependencyConstraint) bool {
	requirement = strings.TrimSpace(requirement)
	requirement = strings.TrimPrefix(requirement, "workspace:")
	for alternative := range strings.SplitSeq(requirement, "||") {
		matched := false
		for _, match := range lowerBoundPattern.FindAllStringSubmatch(alternative, -1) {
			matched = true
			version, _ := versionFromMatch(match[2:])
			if !want.matches(version) {
				return false
			}
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
	case "<":
		return comparison < 0
	default:
		return false
	}
}
