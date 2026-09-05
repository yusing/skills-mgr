package main

import (
	"bytes"
	"errors"
	"fmt"

	"io/fs"
	"os"

	"path/filepath"
	"slices"
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

func newSkillDiscovery(harnesses ...listHarness) skillDiscovery {
	return skillDiscovery{
		seenPaths: make(map[string]struct{}),
		seenNames: make(map[string]struct{}),
		harnesses: harnesses,
	}
}

type skillRoot struct {
	path                           string
	source                         string
	includeSystem                  bool
	editable                       bool
	remoteKey                      string
	disableModelInvocationOverride *bool
	// pluginCache marks a root whose skills live at a nested plugin path rather
	// than directly under it, so one root table can describe both shapes.
	pluginCache bool
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
	discovery := newSkillDiscovery(harnesses...)
	roots := []skillRoot{
		{path: m.paths.projectSkills(project, ".agents"), source: projectSkillSource, editable: true},
		{path: m.paths.userSkills, source: "user", editable: true},
		{path: m.paths.managedSkills, source: managedSkillSource, editable: true},
		{path: m.paths.projectSkills(project, ".claude"), source: "claude", editable: true},
		{path: m.paths.projectSkills(project, ".grok"), source: "grok", editable: true},
		{path: m.paths.projectSkills(project, ".codex"), source: "codex", includeSystem: true, editable: true},
		{path: m.paths.codexSkills(), source: "codex", includeSystem: true, editable: true},
		{path: m.paths.adminSkills, source: "admin"},
		{path: m.paths.codexPluginCache(), pluginCache: true},
	}
	for _, root := range roots {
		if err := discovery.discover(root); err != nil {
			return nil, err
		}
	}
	if m.remoteStore != nil {
		records, err := m.remoteStore.recordsForDiscovery(excludedRemoteKey)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			root, err := m.remoteStore.contentRoot(record)
			if err != nil {
				return nil, err
			}
			before := len(discovery.skills)
			if err := discovery.addSkill(skillRoot{
				source:                         record.Provider,
				editable:                       true,
				remoteKey:                      record.ref().key(),
				disableModelInvocationOverride: record.disableModelInvocationOverride,
			}, root); err != nil {
				return nil, err
			}
			if len(discovery.skills) == before {
				continue
			}
			skill := &discovery.skills[before]
			original, err := os.ReadFile(skill.Path)
			if err != nil {
				return nil, err
			}
			contents, err := m.remoteStore.layeredContent(record.ref(), original)
			if err != nil {
				return nil, err
			}
			frontmatter, _, status, err := readFrontmatter(bytes.NewReader(contents))
			if err != nil {
				return nil, err
			}
			patched, valid := skillFromFrontmatter(frontmatter)
			if status == frontmatterValid && valid {
				// Local wording controls discovery; remote identity and the separate
				// invocation override remain owned by the validated provider record.
				skill.Description = patched.Description
			}
		}
	}
	slices.SortFunc(discovery.skills, compareDiscoveredSkills)
	return discovery.skills, nil
}

func (d *skillDiscovery) discover(root skillRoot) error {
	if root.pluginCache {
		return d.discoverPluginCache(root.path)
	}
	return d.discoverRoot(root)
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
	if marker, err := os.ReadFile(filepath.Join(resolvedRoot, remotePlaceholderMarkerName)); err == nil &&
		string(marker) == remotePlaceholderMarker {
		return nil
	}
	resolvedSkill, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, skillManifestName))
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
	if !skillAllowedForAgent(skill, d.harnesses) {
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
