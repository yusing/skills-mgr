package main

import (
	"errors"
	"fmt"

	"io/fs"
	"os"

	"path/filepath"
	"slices"

	"strings"
)

type discoveredSkill struct {
	Name                   string
	Description            string
	Path                   string
	Root                   string
	EntryPath              string
	Source                 string
	Editable               bool
	RemoteKey              string
	DisableModelInvocation bool
	Plugin                 string
	Vendor                 string
	UserInvocable          bool
	CompatibilityStatus    string
	ExternalEnabled        bool
}

type skillDiscovery struct {
	skills    []discoveredSkill
	seenPaths map[string]struct{}
	seenNames map[string]struct{}
	harnesses []listHarness
}

type skillRoot struct {
	path                           string
	source                         string
	includeSystem                  bool
	editable                       bool
	remoteKey                      string
	disableModelInvocationOverride *bool
}

// Source: ../git-agent/internal/skills/skills.go:67:433 Discover and discovery helpers.
func (m *manager) skills(project string, harnesses ...listHarness) ([]discoveredSkill, error) {
	return m.discoverSkills(project, "", harnesses...)
}

func (m *manager) discoverSkills(
	project string,
	excludedRemoteKey string,
	harnesses ...listHarness,
) ([]discoveredSkill, error) {
	discovery := skillDiscovery{
		seenPaths: make(map[string]struct{}),
		seenNames: make(map[string]struct{}),
		harnesses: harnesses,
	}
	roots := []skillRoot{
		{path: filepath.Join(project, ".agents", "skills"), source: projectSkillSource, editable: true},
		{path: m.paths.userSkills, source: "user", editable: true},
		{path: m.paths.managedSkills, source: managedSkillSource, editable: true},
		{path: filepath.Join(project, ".claude", "skills"), source: "claude", editable: true},
		{path: filepath.Join(project, ".grok", "skills"), source: "grok", editable: true},
		{path: filepath.Join(project, ".codex", "skills"), source: "codex", includeSystem: true, editable: true},
		{path: filepath.Join(m.paths.codexHome, "skills"), source: "codex", includeSystem: true, editable: true},
		{path: m.paths.adminSkills, source: "admin"},
	}
	for _, root := range roots {
		if err := discovery.discoverRoot(root); err != nil {
			return nil, err
		}
	}
	if err := discovery.discoverPluginCache(filepath.Join(m.paths.codexHome, "plugins", "cache")); err != nil {
		return nil, err
	}
	if m.remoteStore != nil {
		records, err := m.remoteStore.recordsForDiscovery(excludedRemoteKey)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			root := filepath.Join(
				m.remoteStore.root,
				filepath.FromSlash(record.Content),
			)
			if err := discovery.addSkill(skillRoot{
				source:                         record.Provider,
				editable:                       true,
				remoteKey:                      record.ref().key(),
				disableModelInvocationOverride: record.disableModelInvocationOverride,
			}, root); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(discovery.skills, func(a, b discoveredSkill) int {
		return strings.Compare(a.Name, b.Name)
	})
	return discovery.skills, nil
}

func (d *skillDiscovery) discoverRoot(root skillRoot) error {
	info, err := os.Stat(root.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil
		}
		return fmt.Errorf("inspect skill root %s: %w", root.path, err)
	}
	if !info.IsDir() {
		return nil
	}
	return d.scanDirectSkillRoot(root)
}

func (d *skillDiscovery) scanDirectSkillRoot(root skillRoot) error {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil
		}
		return fmt.Errorf("read skill root %s: %w", root.path, err)
	}
	for _, entry := range entries {
		path := filepath.Join(root.path, entry.Name())
		if entry.Name() == ".system" && root.includeSystem {
			if err := d.scanDirectSkillRoot(skillRoot{path: path, source: "bundled"}); err != nil {
				return err
			}
			continue
		}
		if err := d.addSkill(root, path); err != nil {
			return err
		}
	}
	return nil
}

func (d *skillDiscovery) discoverPluginCache(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil
		}
		return fmt.Errorf("inspect plugin cache %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrPermission) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil //nolint:nilerr // Skip plugin paths that cannot be relativized.
		}
		if relative != "." && pathDepth(relative) > pluginMaxDepth {
			return filepath.SkipDir
		}
		if entry.Name() != "skills" {
			return nil
		}
		if err := d.scanDirectSkillRoot(skillRoot{path: path, source: "plugin"}); err != nil {
			return err
		}
		return filepath.SkipDir
	})
}

func (d *skillDiscovery) addSkill(root skillRoot, candidateRoot string) error {
	resolvedRoot, err := filepath.EvalSymlinks(candidateRoot)
	if err != nil {
		return nil //nolint:nilerr // Ignore entries that are not usable skill roots.
	}
	if marker, err := os.ReadFile(filepath.Join(resolvedRoot, ".skills-mgr-placeholder")); err == nil &&
		string(marker) == remotePlaceholderMarker {
		return nil
	}
	resolvedSkill, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, "SKILL.md"))
	if err != nil {
		return nil //nolint:nilerr // Ignore roots without a usable SKILL.md.
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedSkill)
	if err != nil || !filepath.IsLocal(relative) {
		return nil //nolint:nilerr // Ignore skill files outside their candidate root.
	}
	skill, ok, err := parseSkill(resolvedSkill)
	if err != nil || !ok {
		return err
	}
	skill.Path = resolvedSkill
	skill.Root = resolvedRoot
	skill.EntryPath = candidateRoot
	skill.Source = root.source
	skill.Editable = root.editable
	skill.RemoteKey = root.remoteKey
	if root.disableModelInvocationOverride != nil {
		skill.DisableModelInvocation = *root.disableModelInvocationOverride
	}
	if !skillAllowedForAgent(skill, d.harnesses, true) {
		return nil
	}
	if _, exists := d.seenPaths[resolvedSkill]; exists {
		return nil
	}
	if _, exists := d.seenNames[skill.Name]; exists {
		return nil
	}
	d.seenPaths[resolvedSkill] = struct{}{}
	d.seenNames[skill.Name] = struct{}{}
	d.skills = append(d.skills, skill)
	return nil
}
