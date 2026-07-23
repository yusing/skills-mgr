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
	skillsMP             *skillsMPRegistry
	runtimeOnce          sync.Once
	javascriptRuntime    string
	javascriptRuntimeErr error
}

type discoveredSkill struct {
	Name        string
	Description string
	Path        string
	Root        string
	Source      string
	Editable    bool
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
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

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
			return nil
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
		return nil
	}
	resolvedSkill, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, "SKILL.md"))
	if err != nil {
		return nil
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedSkill)
	if err != nil || !filepath.IsLocal(relative) {
		return nil
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
	frontmatter, ok, err := readFrontmatter(file)
	if err != nil {
		return discoveredSkill{}, false, err
	}
	if !ok {
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
		Name:        metadata.Name,
		Description: metadata.Description,
	}, true, nil
}

func readFrontmatter(input io.Reader) (string, bool, error) {
	reader := bufio.NewReader(input)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", false, nil
	}
	size := len(line)
	if frontmatterLine(strings.TrimPrefix(line, "\uFEFF")) != "---" {
		return "", false, nil
	}
	var metadata []string
	for {
		line, err = reader.ReadString('\n')
		size += len(line)
		if size > maxFrontmatterBytes {
			return "", false, nil
		}
		normalized := frontmatterLine(line)
		if normalized == "---" {
			return strings.Join(metadata, "\n"), true, nil
		}
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
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
	value, err := loadLock(project)
	if err != nil {
		return nil, err
	}
	return value.Skills, nil
}

func (m *manager) toggle(project, skill string) (bool, error) {
	value, err := loadLock(project)
	if err != nil {
		return false, err
	}
	enabled := !value.Skills[skill]
	value.Skills[skill] = enabled
	if err := saveLock(project, value); err != nil {
		return false, err
	}
	return enabled, nil
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
	available := make(map[string]bool, len(skills))
	for _, skill := range skills {
		available[skill.Name] = true
	}
	for name, enabled := range selected {
		if enabled && !available[name] {
			return fmt.Errorf("enabled skill %q was not discovered", name)
		}
	}
	if _, err := fmt.Fprintln(output, "# Skill list"); err != nil {
		return err
	}
	for _, skill := range skills {
		if !selected[skill.Name] {
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
			if _, err := fmt.Fprintln(output, "\n### references\n\nreferences/"); err != nil {
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
	if lineRange == "" {
		_, err := io.Copy(output, file)
		return err
	}
	start, end, err := parseLineRange(lineRange)
	if err != nil {
		return err
	}
	return writeLineRange(output, file, start, end)
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
	referencesRoot := filepath.Join(skillRoot, "references")
	var files []string
	err := filepath.WalkDir(referencesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			return nil
		}
		relative, err := filepath.Rel(referencesRoot, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
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
