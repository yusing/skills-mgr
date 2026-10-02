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
	ContentMissing         bool
}

type skillDiscovery struct {
	skills     []discoveredSkill
	seenPaths  map[string]struct{}
	seenNames  map[string]struct{}
	targetName string
	harnesses  []listHarness
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

// managementSkills retains absent remote identities for uninstall and refetch
// controls. Access and list continue to use discovery of available content only.
func (m *manager) managementSkills(project, excludedRemoteKey string) ([]discoveredSkill, error) {
	skills, err := m.discoverSkills(project, excludedRemoteKey)
	if err != nil || m.remoteStore == nil {
		return skills, err
	}
	records, err := m.remoteStore.records()
	if err != nil {
		return nil, err
	}
	availableCount := len(skills)
	for _, record := range records {
		if record.ref().key() == excludedRemoteKey {
			continue
		}
		if _, err := m.remoteStore.contentRoot(record); errors.Is(err, errRemoteSkillContentMissing) {
			skills = append(skills, discoveredSkill{
				Name: record.Name, Source: record.Provider, RemoteKey: record.ref().key(),
				Description:    "Cached content is missing. Use sync or reinstall to restore it, or u to uninstall.",
				ContentMissing: true,
			})
		} else if err != nil {
			return nil, err
		}
	}
	if len(skills) != availableCount {
		slices.SortFunc(skills, compareDiscoveredSkills)
	}
	return skills, nil
}

func (m *manager) discoverSkills(
	project string,
	excludedRemoteKey string,
	harnesses ...listHarness,
) ([]discoveredSkill, error) {
	return m.discoverSkillsNamed(project, excludedRemoteKey, "", harnesses...)
}

// discoverSkillsNamed stops at the first filesystem owner when a name is given.
// Directory names need not match frontmatter names, so each preceding entry is
// still inspected in the same order as full discovery.
func (m *manager) discoverSkillsNamed(
	project, excludedRemoteKey, name string,
	harnesses ...listHarness,
) ([]discoveredSkill, error) {
	discovery := newSkillDiscovery(harnesses...)
	discovery.targetName = name
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
		if name != "" && len(discovery.skills) > 0 {
			return discovery.skills, nil
		}
	}
	if m.remoteStore != nil {
		records, err := m.remoteStore.recordsForDiscoveryNamed(excludedRemoteKey, name)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			root, err := m.remoteStore.contentRoot(record)
			if err != nil {
				return nil, err
			}
			before := len(discovery.skills)
			if err := discovery.addResolvedSkill(skillRoot{
				source:                         record.Provider,
				editable:                       true,
				remoteKey:                      record.ref().key(),
				disableModelInvocationOverride: record.disableModelInvocationOverride,
			}, root, root); err != nil {
				return nil, err
			}
			if len(discovery.skills) == before {
				continue
			}
			if name != "" {
				// Access applies the patch when it reads the requested SKILL.md;
				// it does not need the catalog's patched description.
				return discovery.skills, nil
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
			if bytes.Equal(contents, original) {
				continue
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
	if !skillAllowedForAgent(discoveredSkill{Source: root.source}, d.harnesses) ||
		(root.pluginCache && !skillAllowedForAgent(discoveredSkill{Source: "plugin"}, d.harnesses)) {
		return nil
	}
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
	resolvedParent, err := filepath.EvalSymlinks(root.path)
	if err != nil {
		return nil //nolint:nilerr // Ignore roots that disappeared during discovery.
	}
	for _, entry := range entries {
		path := filepath.Join(root.path, entry.Name())
		if entry.Name() == ".system" && root.includeSystem {
			if err := d.scanDirectSkillRoot(skillRoot{path: path, source: "bundled"}); err != nil {
				return err
			}
			if d.targetName != "" && len(d.skills) > 0 {
				return nil
			}
			continue
		}
		var err error
		if entry.IsDir() {
			err = d.addResolvedSkill(root, path, filepath.Join(resolvedParent, entry.Name()))
		} else if entry.Type()&os.ModeSymlink != 0 {
			err = d.addSkill(root, path)
		}
		if err != nil {
			return err
		}
		if d.targetName != "" && len(d.skills) > 0 {
			return nil
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
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
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
		if d.targetName != "" && len(d.skills) > 0 {
			return fs.SkipAll
		}
		return filepath.SkipDir
	})
	return err
}

func (d *skillDiscovery) addSkill(root skillRoot, candidateRoot string) error {
	resolvedRoot, err := filepath.EvalSymlinks(candidateRoot)
	if err != nil {
		return nil //nolint:nilerr // Ignore entries that are not usable skill roots.
	}
	return d.addResolvedSkill(root, candidateRoot, resolvedRoot)
}

func (d *skillDiscovery) addResolvedSkill(root skillRoot, candidateRoot, resolvedRoot string) error {
	if marker, err := os.ReadFile(filepath.Join(resolvedRoot, remotePlaceholderMarkerName)); err == nil &&
		string(marker) == remotePlaceholderMarker {
		return nil
	}
	manifest := filepath.Join(resolvedRoot, skillManifestName)
	info, err := os.Lstat(manifest)
	if err != nil {
		return nil //nolint:nilerr // Ignore roots without a usable SKILL.md.
	}
	resolvedSkill := manifest
	if info.Mode()&os.ModeSymlink != 0 {
		resolvedSkill, err = filepath.EvalSymlinks(manifest)
		if err != nil {
			return nil //nolint:nilerr // Ignore roots without a usable SKILL.md.
		}
		relative, err := filepath.Rel(resolvedRoot, resolvedSkill)
		if err != nil || !filepath.IsLocal(relative) {
			return nil //nolint:nilerr // Ignore skill files outside their candidate root.
		}
		info, err = os.Stat(resolvedSkill)
		if err != nil {
			return nil //nolint:nilerr // Ignore skill files that disappeared.
		}
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	skill, ok, err := parseSkill(resolvedSkill)
	if err != nil || !ok {
		return err
	}
	if d.targetName != "" && skill.Name != d.targetName {
		return nil
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
