package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
)

const (
	pluginMaxDepth      = 10
	maxSkillNameLen     = 80
	maxSkillDescLen     = 300
	maxFrontmatterBytes = 64 * 1024
)

type manager struct {
	paths                paths
	remote               *remoteRegistry
	skillsMP             *skillsMPRegistry
	remoteStore          *remoteSkillStore
	global               bool
	runtimeOnce          sync.Once
	javascriptRuntime    string
	javascriptRuntimeErr error
}

type discoveredSkill struct {
	Name                   string
	Description            string
	Path                   string
	Root                   string
	Source                 string
	Editable               bool
	RemoteKey              string
	DisableModelInvocation bool
}

type skillDiscovery struct {
	skills    []discoveredSkill
	seenPaths map[string]struct{}
	seenNames map[string]struct{}
}

type skillRoot struct {
	path          string
	source        string
	includeSystem bool
	editable      bool
	remoteKey     string
}

type skillFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

type frontmatterStatus uint8

const (
	frontmatterAbsent frontmatterStatus = iota
	frontmatterValid
	frontmatterMalformed
)

// Source: ../git-agent/internal/skills/skills.go:67:433 Discover and discovery helpers.
func (m *manager) skills(project string) ([]discoveredSkill, error) {
	discovery := skillDiscovery{
		seenPaths: make(map[string]struct{}),
		seenNames: make(map[string]struct{}),
	}
	roots := []skillRoot{
		{path: filepath.Join(project, ".agents", "skills"), source: "project", editable: true},
		{path: m.paths.userSkills, source: "user", editable: true},
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
		records, err := m.remoteStore.records()
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			root := filepath.Join(
				m.remoteStore.root,
				filepath.FromSlash(record.Content),
			)
			if err := discovery.addSkill(skillRoot{
				source:    record.Provider,
				remoteKey: record.ref().key(),
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
	if _, exists := d.seenPaths[resolvedSkill]; exists {
		return nil
	}
	if _, exists := d.seenNames[skill.Name]; exists {
		return nil
	}
	skill.Path = resolvedSkill
	skill.Root = resolvedRoot
	skill.Source = root.source
	skill.Editable = root.editable
	skill.RemoteKey = root.remoteKey
	d.seenPaths[resolvedSkill] = struct{}{}
	d.seenNames[skill.Name] = struct{}{}
	d.skills = append(d.skills, skill)
	return nil
}

func parseSkill(path string) (discoveredSkill, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return discoveredSkill{}, false, nil
		}
		return discoveredSkill{}, false, err
	}
	defer file.Close()
	frontmatter, _, status, err := readFrontmatter(file)
	if err != nil {
		return discoveredSkill{}, false, err
	}
	if status != frontmatterValid {
		return discoveredSkill{}, false, nil
	}
	var metadata skillFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		return discoveredSkill{}, false, nil
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = truncateMetadata(
		strings.Join(strings.Fields(metadata.Description), " "),
		maxSkillDescLen,
	)
	if !validSkillName(metadata.Name) || !validSkillDescription(metadata.Description) {
		return discoveredSkill{}, false, nil
	}
	return discoveredSkill{
		Name:                   metadata.Name,
		Description:            metadata.Description,
		DisableModelInvocation: metadata.DisableModelInvocation,
	}, true, nil
}

func readFrontmatter(input io.Reader) (string, io.Reader, frontmatterStatus, error) {
	reader := bufio.NewReader(input)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", nil, frontmatterMalformed, err
	}
	if frontmatterLine(strings.TrimPrefix(line, "\uFEFF")) != "---" {
		return "", io.MultiReader(strings.NewReader(line), reader), frontmatterAbsent, nil
	}
	if errors.Is(err, io.EOF) {
		return "", nil, frontmatterMalformed, nil
	}
	size := len(line)
	var metadata []string
	for {
		line, err = reader.ReadString('\n')
		size += len(line)
		if size > maxFrontmatterBytes {
			return "", nil, frontmatterMalformed, nil
		}
		normalized := frontmatterLine(line)
		if normalized == "---" {
			return strings.Join(metadata, "\n"), reader, frontmatterValid, nil
		}
		if errors.Is(err, io.EOF) {
			return "", nil, frontmatterMalformed, nil
		}
		if err != nil {
			return "", nil, frontmatterMalformed, err
		}
		metadata = append(metadata, normalized)
	}
}

func frontmatterLine(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

func validSkillName(name string) bool {
	if name == "" || len(name) > maxSkillNameLen {
		return false
	}
	for _, character := range name {
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '-' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func validSkillDescription(description string) bool {
	if description == "" {
		return false
	}
	for _, character := range description {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func truncateMetadata(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	text = text[:maxBytes]
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return strings.TrimSpace(text)
}

func pathDepth(relative string) int {
	if relative == "." || relative == "" {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(relative), "/"))
}

func (m *manager) selection(project string) (map[string]bool, error) {
	selected, _, _, err := m.selectionLayers(project)
	return selected, err
}

func (m *manager) selectionLayers(
	project string,
) (
	selected map[string]bool,
	globalSelected map[string]bool,
	projectSelected map[string]bool,
	err error,
) {
	global, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return nil, nil, nil, err
	}
	if m.global {
		return global.Skills, nil, nil, nil
	}
	projectLock, err := loadLock(project)
	if err != nil {
		return nil, nil, nil, err
	}
	return mergeSelections(global.Skills, projectLock.Skills),
		global.Skills,
		projectLock.Skills,
		nil
}

func (m *manager) lockDir(project string) string {
	if m.global {
		return m.paths.globalLockDir
	}
	return project
}

func (m *manager) toggle(project, skill string, remoteKey ...string) (bool, error) {
	var remoteRef *remoteSkillRef
	if len(remoteKey) > 0 && remoteKey[0] != "" {
		ref, err := m.persistedRemoteRef(remoteKey[0], skill)
		if err != nil {
			return false, err
		}
		remoteRef = new(ref)
	}

	enabled := false
	previousEnabled := false
	previousExists := false
	var previousRemote remoteSkillRef
	previousRemoteExists := false
	err := m.updateSelectionLock(project, func(value *lock) (bool, error) {
		selected := value.Skills
		if !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			selected = mergeSelections(global.Skills, value.Skills)
		}
		enabled = !selected[skill]
		previousEnabled, previousExists = value.Skills[skill]
		previousRemote, previousRemoteExists = value.Remote[skill]
		value.Skills[skill] = enabled
		if remoteRef == nil {
			delete(value.Remote, skill)
		} else {
			value.Remote[skill] = *remoteRef
		}

		return true, nil
	})
	if err != nil {
		return false, err
	}
	if remoteRef == nil {
		return enabled, nil
	}
	description, placeholderErr := m.remoteSkillDescription(*remoteRef)
	if placeholderErr == nil {
		placeholderErr = m.setRemotePlaceholders(project, skill, description, enabled)
	}
	if placeholderErr != nil {
		rollbackErr := m.updateSelectionLock(project, func(value *lock) (bool, error) {
			if value.Skills[skill] != enabled || value.Remote[skill] != *remoteRef {
				return false, fmt.Errorf("remote selection changed during placeholder rollback")
			}
			if previousExists {
				value.Skills[skill] = previousEnabled
			} else {
				delete(value.Skills, skill)
			}
			if previousRemoteExists {
				value.Remote[skill] = previousRemote
			} else {
				delete(value.Remote, skill)
			}
			return true, nil
		})
		return false, errors.Join(placeholderErr, rollbackErr)
	}
	return enabled, nil
}

func (m *manager) setRemoteSelection(
	project string,
	ref remoteSkillRef,
	enabled bool,
) (map[string]bool, error) {
	var selected map[string]bool
	err := m.updateSelectionLock(project, func(value *lock) (bool, error) {
		value.Skills[ref.Name] = enabled
		value.Remote[ref.Name] = ref
		selected = value.Skills
		if !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			selected = mergeSelections(global.Skills, value.Skills)
		}
		return true, nil
	})
	return selected, err
}

func (m *manager) updateSelectionLock(
	project string,
	update func(*lock) (bool, error),
) error {
	lockDir := m.lockDir(project)
	updateMigrated := func(value *lock) (bool, error) {
		migrated, err := m.migrateSelectionLock(value)
		if err != nil {
			return false, err
		}
		changed, err := update(value)
		return migrated || changed, err
	}
	return updateLock(lockDir, m.paths.selectionLocks, updateMigrated)
}

func (m *manager) migrateSelectionLock(value *lock) (bool, error) {
	if value.SchemaRevision != legacyLockSchemaRevision {
		return false, nil
	}
	refs, err := m.persistedRemoteRefs()
	if err != nil {
		return false, err
	}
	global := newLock()
	if !m.global {
		global, err = loadLock(m.paths.globalLockDir)
		if err != nil {
			return false, err
		}
	}
	if _, err := reconcileRemoteMetadata(global, value, refs); err != nil {
		return false, err
	}
	value.SchemaRevision = lockSchemaRevision
	return true, nil
}

func (m *manager) persistedRemoteRefs() (map[string]remoteSkillRef, error) {
	if m.remoteStore == nil {
		return make(map[string]remoteSkillRef), nil
	}
	records, err := m.remoteStore.records()
	if err != nil {
		return nil, err
	}
	refs := make(map[string]remoteSkillRef, len(records))
	for _, record := range records {
		ref := record.ref()
		if existing, ok := refs[ref.Name]; ok && existing != ref {
			return nil, fmt.Errorf(
				"multiple persisted remote identities for skill %q",
				ref.Name,
			)
		}
		refs[ref.Name] = ref
	}
	return refs, nil
}

func (m *manager) persistedRemoteRef(
	key string,
	name string,
) (remoteSkillRef, error) {
	if m.remoteStore == nil {
		return remoteSkillRef{}, fmt.Errorf("remote skill store is unavailable")
	}
	records, err := m.remoteStore.records()
	if err != nil {
		return remoteSkillRef{}, err
	}
	for _, record := range records {
		ref := record.ref()
		if ref.key() != key {
			continue
		}
		if ref.Name != name {
			return remoteSkillRef{}, fmt.Errorf(
				"remote skill metadata for %q belongs to skill %q",
				key,
				ref.Name,
			)
		}
		return ref, nil
	}
	return remoteSkillRef{}, fmt.Errorf(
		"remote skill metadata for %q is unavailable",
		name,
	)
}

func reconcileRemoteMetadata(
	globalLock lock,
	projectLock *lock,
	refs map[string]remoteSkillRef,
) (bool, error) {
	changed := false
	add := func(name string) error {
		ref, ok := refs[name]
		if !ok {
			return nil
		}
		if existing, ok := projectLock.Remote[name]; ok {
			if existing != ref {
				return fmt.Errorf(
					"remote skill %q identity conflicts with persisted reference",
					name,
				)
			}
			return nil
		}
		projectLock.Remote[name] = ref
		changed = true
		return nil
	}
	for name := range projectLock.Skills {
		if err := add(name); err != nil {
			return false, err
		}
	}
	for name := range globalLock.Skills {
		if _, overridden := projectLock.Skills[name]; overridden {
			continue
		}
		if err := add(name); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func mergeSelections(global, project map[string]bool) map[string]bool {
	selected := maps.Clone(global)
	maps.Copy(selected, project)
	return selected
}

func (m *manager) list(project string, output io.Writer) error {
	selected, err := m.selection(project)
	if err != nil {
		return err
	}
	skills, err := m.skills(project)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "# Skill list"); err != nil {
		return err
	}
	for _, skill := range skills {
		if !selected[skill.Name] || skill.DisableModelInvocation {
			continue
		}
		references, err := referenceFiles(skill.Root)
		if err != nil {
			return fmt.Errorf("%s: %w", skill.Name, err)
		}
		if _, err := fmt.Fprintf(output, "\n## %s\n\n%s\n", skill.Name, skill.Description); err != nil {
			return err
		}
		if len(references) > 0 {
			if _, err := fmt.Fprint(output, "\n### references\n\n"); err != nil {
				return err
			}
			if err := writeReferenceTree(output, references); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *manager) get(project, target, lineRange string, output io.Writer) error {
	skill, relative, err := splitTarget(target)
	if err != nil {
		return err
	}
	root, err := m.openSkill(project, skill)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(relative))
	if err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", target)
	}
	var start, end int
	if lineRange != "" {
		start, end, err = parseLineRange(lineRange)
		if err != nil {
			return err
		}
	}
	var input io.Reader = file
	if strings.EqualFold(filepath.Ext(relative), ".md") {
		_, body, status, err := readFrontmatter(file)
		if err != nil {
			return fmt.Errorf("read %s frontmatter: %w", target, err)
		}
		switch status {
		case frontmatterValid:
			input = body
		case frontmatterAbsent:
			if filepath.Clean(filepath.FromSlash(relative)) == "SKILL.md" {
				return fmt.Errorf("%s has invalid frontmatter", target)
			}
			input = body
		case frontmatterMalformed:
			return fmt.Errorf("%s has invalid frontmatter", target)
		}
	}
	if lineRange == "" {
		_, err := io.Copy(output, input)
		return err
	}
	return writeLineRange(output, input, start, end)
}

func (m *manager) scriptCommand(project, target string, args []string) (*exec.Cmd, error) {
	if !strings.Contains(target, "/") {
		return nil, fmt.Errorf("invalid script target %q; want <skill-name>/<relative/script>", target)
	}
	skill, relative, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	root, err := m.openSkill(project, skill)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(filepath.FromSlash(relative))
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", target)
	}

	script := filepath.Join(root.Name(), filepath.FromSlash(relative))
	extension := filepath.Ext(relative)
	var command *exec.Cmd
	switch {
	case info.Mode()&0o111 != 0:
		command = exec.Command(script, args...)
	case extension == ".py":
		command = exec.Command("python3", append([]string{script}, args...)...)
	case isJavaScript(extension):
		runtime, err := m.cachedJavaScriptRuntime()
		if err != nil {
			return nil, err
		}
		command = exec.Command(runtime, append([]string{script}, args...)...)
	default:
		command = exec.Command(script, args...)
	}
	if command.Err != nil {
		return nil, fmt.Errorf("run %s: %w", target, command.Err)
	}
	command.Dir = root.Name()
	return command, nil
}

func isJavaScript(extension string) bool {
	switch extension {
	case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts":
		return true
	default:
		return false
	}
}

func (m *manager) cachedJavaScriptRuntime() (string, error) {
	m.runtimeOnce.Do(func() {
		for _, name := range []string{"node", "bun"} {
			path, err := exec.LookPath(name)
			if err == nil {
				m.javascriptRuntime = path
				return
			}
		}
		m.javascriptRuntimeErr = fmt.Errorf("run JavaScript or TypeScript: neither node nor bun was found in PATH")
	})
	return m.javascriptRuntime, m.javascriptRuntimeErr
}

func (m *manager) openSkill(project, skill string) (*os.Root, error) {
	selected, err := m.selection(project)
	if err != nil {
		return nil, err
	}
	if !selected[skill] {
		return nil, fmt.Errorf("skill %q is not enabled", skill)
	}
	discovered, err := m.findSkill(project, skill)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(discovered.Root)
	if err != nil {
		return nil, fmt.Errorf("open skill %q: %w", skill, err)
	}
	return root, nil
}

func (m *manager) findSkill(project, name string) (discoveredSkill, error) {
	skills, err := m.skills(project)
	if err != nil {
		return discoveredSkill{}, err
	}
	for _, skill := range skills {
		if skill.Name == name {
			return skill, nil
		}
	}
	return discoveredSkill{}, fmt.Errorf("skill %q was not discovered", name)
}

func splitTarget(target string) (skill, relative string, err error) {
	skill, relative, hasRelative := strings.Cut(target, "/")
	if skill == "" || skill == "." || !filepath.IsLocal(skill) {
		return "", "", fmt.Errorf("invalid skill target %q", target)
	}
	if !hasRelative {
		return skill, "SKILL.md", nil
	}
	if relative == "" || !filepath.IsLocal(filepath.FromSlash(relative)) {
		return "", "", fmt.Errorf("invalid skill target %q", target)
	}
	return skill, relative, nil
}

func parseLineRange(value string) (int, int, error) {
	startText, endText, ok := strings.Cut(value, ":")
	if !ok || startText == "" || endText == "" {
		return 0, 0, fmt.Errorf("invalid line range %q; want start:end", value)
	}
	start, startErr := strconv.Atoi(startText)
	end, endErr := strconv.Atoi(endText)
	if startErr != nil || endErr != nil || start < 1 || end < start {
		return 0, 0, fmt.Errorf("invalid line range %q; want positive start:end with start <= end", value)
	}
	return start, end, nil
}

func writeLineRange(output io.Writer, input io.Reader, start, end int) error {
	reader := bufio.NewReader(input)
	for lineNumber := 1; ; lineNumber++ {
		line, err := reader.ReadString('\n')
		if lineNumber >= start && lineNumber <= end && len(line) > 0 {
			if _, writeErr := io.WriteString(output, line); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			if lineNumber < start || lineNumber == start && len(line) == 0 {
				return fmt.Errorf("line %d is beyond end of file", start)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if lineNumber == end {
			return nil
		}
	}
}

func referenceFiles(skillRoot string) ([]string, error) {
	entries, err := os.ReadDir(skillRoot)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "SKILL.md" || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		files = append(files, entry.Name())
	}

	referencesRoot := filepath.Join(skillRoot, "references")
	err = filepath.WalkDir(referencesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			return nil
		}
		relative, err := filepath.Rel(skillRoot, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	return files, err
}

type referenceNode struct {
	children map[string]*referenceNode
}

func writeReferenceTree(output io.Writer, files []string) error {
	root := &referenceNode{children: make(map[string]*referenceNode)}
	for _, file := range files {
		node := root
		for part := range strings.SplitSeq(file, "/") {
			if node.children[part] == nil {
				node.children[part] = &referenceNode{children: make(map[string]*referenceNode)}
			}
			node = node.children[part]
		}
	}
	return writeReferenceNodes(output, root, "")
}

func writeReferenceNodes(output io.Writer, node *referenceNode, prefix string) error {
	names := slices.Sorted(maps.Keys(node.children))
	for index, name := range names {
		child := node.children[name]
		last := index == len(names)-1
		branch := "├── "
		nextPrefix := prefix + "│   "
		if last {
			branch = "└── "
			nextPrefix = prefix + "    "
		}
		suffix := ""
		if len(child.children) > 0 {
			suffix = "/"
		}
		if _, err := fmt.Fprintf(output, "%s%s%s%s\n", prefix, branch, name, suffix); err != nil {
			return err
		}
		if err := writeReferenceNodes(output, child, nextPrefix); err != nil {
			return err
		}
	}
	return nil
}
