package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
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
	"syscall"
	"time"
	"unicode"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	yamlToken "github.com/goccy/go-yaml/token"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

const (
	pluginMaxDepth      = 10
	maxSkillNameLen     = 80
	maxFrontmatterBytes = 64 * 1024
	projectSkillSource  = "project"
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

type skillFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

type modelInvocationResult struct {
	Skill           string
	RemoteKey       string
	Disabled        bool
	Skills          []discoveredSkill
	Selected        map[string]bool
	GlobalSelected  map[string]bool
	ProjectSelected map[string]bool
}

type frontmatterStatus uint8

const (
	frontmatterAbsent frontmatterStatus = iota
	frontmatterValid
	frontmatterMalformed
)

// Source: ../git-agent/internal/skills/skills.go:67:433 Discover and discovery helpers.
func (m *manager) skills(project string, harnesses ...listHarness) ([]discoveredSkill, error) {
	discovery := skillDiscovery{
		seenPaths: make(map[string]struct{}),
		seenNames: make(map[string]struct{}),
		harnesses: harnesses,
	}
	roots := []skillRoot{
		{path: filepath.Join(project, ".agents", "skills"), source: projectSkillSource, editable: true},
		{path: m.paths.userSkills, source: "user", editable: true},
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
		records, err := m.remoteStore.recordsForDiscovery()
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
	metadata.Description = strings.Join(strings.Fields(metadata.Description), " ")
	if !validSkillName(metadata.Name) || !validSkillDescription(metadata.Description) {
		return discoveredSkill{}, false, nil
	}
	return discoveredSkill{
		Name:                   metadata.Name,
		Description:            metadata.Description,
		DisableModelInvocation: metadata.DisableModelInvocation,
	}, true, nil
}

func toggleModelInvocationFrontmatter(data []byte) ([]byte, bool, error) {
	frontmatter, body, status, err := readFrontmatter(bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	if status != frontmatterValid {
		return nil, false, fmt.Errorf("SKILL.md has invalid frontmatter")
	}
	bodyLength, err := io.Copy(io.Discard, body)
	if err != nil {
		return nil, false, err
	}
	metadataStart := bytes.IndexByte(data, '\n') + 1
	metadataEnd := metadataStart + len(frontmatter)
	if metadataStart == 0 || metadataEnd > len(data)-int(bodyLength) {
		return nil, false, fmt.Errorf("SKILL.md frontmatter boundaries are invalid")
	}

	frontmatterBytes := []byte(frontmatter)
	file, err := parser.ParseBytes(frontmatterBytes, parser.ParseComments)
	if err != nil {
		return nil, false, fmt.Errorf("parse SKILL.md frontmatter: %w", err)
	}
	if len(file.Docs) != 1 {
		return nil, false, fmt.Errorf("SKILL.md frontmatter must contain one document")
	}
	mapping, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, false, fmt.Errorf("SKILL.md frontmatter must be a mapping")
	}
	var metadata skillFrontmatter
	if err := yaml.Unmarshal(frontmatterBytes, &metadata); err != nil {
		return nil, false, fmt.Errorf("decode SKILL.md frontmatter: %w", err)
	}
	disabled := !metadata.DisableModelInvocation
	tokenOffset := func(current *yamlToken.Token, value string) (int, error) {
		start := strings.Index(current.Origin, value)
		if start < 0 {
			return 0, fmt.Errorf("locate YAML token")
		}
		for previous := current.Prev; previous != nil; previous = previous.Prev {
			start += len(previous.Origin)
		}
		return start, nil
	}
	preserveBooleanCase := func(original, replacement string) string {
		switch {
		case original == strings.ToUpper(original):
			return strings.ToUpper(replacement)
		case original == strings.ToLower(original):
			return replacement
		case len(original) > 1 &&
			original[:1] == strings.ToUpper(original[:1]) &&
			original[1:] == strings.ToLower(original[1:]):
			return strings.ToUpper(replacement[:1]) + replacement[1:]
		default:
			return replacement
		}
	}
	taggedBooleanReplacement := func(value string) (string, bool) {
		switch strings.ToLower(value) {
		case "yes", "no":
			replacement := "no"
			if disabled {
				replacement = "yes"
			}
			return preserveBooleanCase(value, replacement), true
		case "true", "false":
			return preserveBooleanCase(value, strconv.FormatBool(disabled)), true
		case "t", "f":
			replacement := "f"
			if disabled {
				replacement = "t"
			}
			return preserveBooleanCase(value, replacement), true
		case "1", "0":
			if disabled {
				return "1", true
			}
			return "0", true
		default:
			return "", false
		}
	}
	type edit struct {
		start       int
		end         int
		replacement string
	}
	var edits []edit
	for _, entry := range mapping.Values {
		var key string
		if err := yaml.NodeToValue(entry.Key, &key); err != nil {
			return nil, false, fmt.Errorf("decode SKILL.md frontmatter key: %w", err)
		}
		if key != "disable-model-invocation" {
			continue
		}
		node := entry.Value
		taggedBoolean := false
		var scalar *yamlToken.Token
		for scalar == nil {
			switch current := node.(type) {
			case *ast.AnchorNode:
				node = current.Value
			case *ast.TagNode:
				taggedBoolean = taggedBoolean || current.GetToken().Value == "!!bool"
				node = current.Value
			case *ast.BoolNode:
				scalar = current.GetToken()
			case *ast.StringNode, *ast.IntegerNode:
				if !taggedBoolean {
					return nil, false, fmt.Errorf(
						"disable-model-invocation must be a boolean value, not %T",
						node,
					)
				}
				scalar = current.GetToken()
			default:
				return nil, false, fmt.Errorf(
					"disable-model-invocation must be a boolean value, not %T",
					node,
				)
			}
		}
		replacement := strconv.FormatBool(disabled)
		if taggedBoolean {
			var ok bool
			replacement, ok = taggedBooleanReplacement(scalar.Value)
			if !ok {
				return nil, false, fmt.Errorf(
					"disable-model-invocation has unsupported !!bool value %q",
					scalar.Value,
				)
			}
		}
		start, err := tokenOffset(scalar, scalar.Value)
		if err != nil {
			return nil, false, fmt.Errorf("locate disable-model-invocation token: %w", err)
		}
		end := start + len(scalar.Value)
		if end > len(frontmatterBytes) || string(frontmatterBytes[start:end]) != scalar.Value {
			return nil, false, fmt.Errorf("locate disable-model-invocation value")
		}
		edits = append(edits, edit{start: start, end: end, replacement: replacement})
	}
	lineEnding := "\n"
	if bytes.HasSuffix(data[:metadataStart], []byte("\r\n")) {
		lineEnding = "\r\n"
	}
	rendered := slices.Clone(frontmatterBytes)
	if len(edits) == 0 {
		if mapping.IsFlowStyle {
			if mapping.End == nil {
				return nil, false, fmt.Errorf("locate flow-style frontmatter closing brace")
			}
			closing, err := tokenOffset(mapping.End, mapping.End.Value)
			if err != nil || closing >= len(rendered) || rendered[closing] != '}' {
				return nil, false, fmt.Errorf("locate flow-style frontmatter closing brace")
			}
			addition := fmt.Appendf(
				nil,
				", disable-model-invocation: %t",
				disabled,
			)
			rendered = slices.Insert(rendered, closing, addition...)
		} else {
			if len(rendered) > 0 && !bytes.HasSuffix(rendered, []byte(lineEnding)) {
				rendered = append(rendered, lineEnding...)
			}
			rendered = fmt.Appendf(
				rendered,
				"disable-model-invocation: %t%s",
				disabled,
				lineEnding,
			)
		}
	} else {
		slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
		for _, edit := range slices.Backward(edits) {
			rendered = slices.Replace(
				rendered,
				edit.start,
				edit.end,
				[]byte(edit.replacement)...,
			)
		}
	}
	updated := make([]byte, 0, len(data)+len(rendered)-len(frontmatterBytes))
	updated = append(updated, data[:metadataStart]...)
	updated = append(updated, rendered...)
	updated = append(updated, data[metadataEnd:]...)
	return updated, disabled, nil
}

func toggleModelInvocationFile(
	ctx context.Context,
	coordinationDir string,
	path string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(coordinationDir, 0o700); err != nil {
		return false, fmt.Errorf("create model invocation lock directory: %w", err)
	}
	lockKey := sha256.Sum256([]byte(path))
	lockPath := filepath.Join(
		coordinationDir,
		fmt.Sprintf("skill-model-invocation-%x.lock", lockKey),
	)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open model invocation lock: %w", err)
	}
	defer lock.Close()
acquire:
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			break acquire
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, ctx.Err()
			case <-timer.C:
				continue
			}
		default:
			return false, fmt.Errorf("lock model invocation update: %w", err)
		}
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("skill source %s is not a regular file", path)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	updated, disabled, err := toggleModelInvocationFrontmatter(original)
	if err != nil {
		return false, err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".skill-model-invocation-")
	if err != nil {
		return false, fmt.Errorf("create temporary SKILL.md: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	err = temporary.Chmod(info.Mode().Perm())
	if err == nil {
		_, err = temporary.Write(updated)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err != nil {
		return false, fmt.Errorf("write temporary SKILL.md: %w", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("recheck %s: %w", path, err)
	}
	currentInfo, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("recheck %s: %w", path, err)
	}
	if !os.SameFile(info, currentInfo) || currentInfo.Mode() != info.Mode() ||
		!bytes.Equal(current, original) {
		return false, fmt.Errorf("skill source changed while model invocation was updating")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.Rename(name, path); err != nil {
		return false, fmt.Errorf("replace %s: %w", path, err)
	}
	return disabled, nil
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
	var metadata strings.Builder
	for {
		line, err = reader.ReadString('\n')
		size += len(line)
		if size > maxFrontmatterBytes {
			return "", nil, frontmatterMalformed, nil
		}
		normalized := frontmatterLine(line)
		if normalized == "---" {
			return metadata.String(), reader, frontmatterValid, nil
		}
		if errors.Is(err, io.EOF) {
			return "", nil, frontmatterMalformed, nil
		}
		if err != nil {
			return "", nil, frontmatterMalformed, err
		}
		metadata.WriteString(line)
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

func pathDepth(relative string) int {
	if relative == "." || relative == "" {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(relative), "/"))
}

type selectionState struct {
	selected           map[string]bool
	globalSelected     map[string]bool
	projectSelected    map[string]bool
	expressions        map[string]string
	globalExpressions  map[string]string
	projectExpressions map[string]string
}

func (s selectionState) enabled(
	ctx context.Context,
	evaluator *enabledEvaluator,
	name string,
) (bool, error) {
	if !s.selected[name] {
		return false, nil
	}
	expression, conditional := s.expressions[name]
	if !conditional {
		return true, nil
	}
	return evaluator.evaluate(ctx, name, expression)
}

func (m *manager) selectionState(
	project string,
	catalog []discoveredSkill,
) (selectionState, error) {
	global, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return selectionState{}, err
	}
	if m.global {
		selected := configuredSelections(global)
		return selectionState{
			selected:          selected,
			expressions:       maps.Clone(global.Expressions),
			globalExpressions: maps.Clone(global.Expressions),
		}, nil
	}
	projectLock, err := loadLock(project)
	if err != nil {
		return selectionState{}, err
	}
	selected, expressions := mergeSelectionLocks(global, projectLock)
	var skills []discoveredSkill
	if catalog == nil {
		skills, err = m.skills(project)
		if err != nil {
			return selectionState{}, err
		}
	} else {
		skills = catalog
	}
	for _, skill := range skills {
		if skillEnabled(selected, skill) {
			selected[skill.Name] = true
		}
	}
	return selectionState{
		selected:           selected,
		globalSelected:     configuredSelections(global),
		projectSelected:    configuredSelections(projectLock),
		expressions:        expressions,
		globalExpressions:  maps.Clone(global.Expressions),
		projectExpressions: maps.Clone(projectLock.Expressions),
	}, nil
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

	target := discoveredSkill{Name: skill}
	if !m.global {
		skills, err := m.skills(project)
		if err != nil {
			return false, err
		}
		for _, discovered := range skills {
			if discovered.Name == skill {
				target = discovered
				break
			}
		}
	}

	enabled := false
	var previousEnabled enabledValue
	previousExists := false
	var previousRemote remoteSkillRef
	previousRemoteExists := false
	err := m.updateSelectionLock(project, func(value *lock) (bool, error) {
		selected := configuredSelections(*value)
		if !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			selected, _ = mergeSelectionLocks(global, *value)
		}
		enabled = !skillEnabled(selected, target)
		if enabled && m.global && remoteRef != nil {
			if err := m.validatePersistedRemoteRef(*remoteRef); err != nil {
				return false, err
			}
		}
		previousEnabled, previousExists = value.enabled(skill)
		previousRemote, previousRemoteExists = value.Remote[skill]
		value.setEnabled(skill, enabledValue{Boolean: new(enabled)})
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
	frontmatter, placeholderErr := m.remoteSkillFrontmatter(*remoteRef)
	if placeholderErr == nil {
		placeholderErr = m.setRemotePlaceholders(project, skill, frontmatter, enabled)
	}
	if placeholderErr != nil {
		rollbackErr := m.updateSelectionLock(project, func(value *lock) (bool, error) {
			current, exists := value.enabled(skill)
			if !exists || current.Boolean == nil ||
				*current.Boolean != enabled ||
				value.Remote[skill] != *remoteRef {
				return false, fmt.Errorf("remote selection changed during placeholder rollback")
			}
			if previousExists {
				value.setEnabled(skill, previousEnabled)
			} else {
				value.deleteEnabled(skill)
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
		if enabled && m.global {
			if err := m.validatePersistedRemoteRef(ref); err != nil {
				return false, err
			}
		}
		value.setEnabled(ref.Name, enabledValue{Boolean: new(enabled)})
		value.Remote[ref.Name] = ref
		selected = configuredSelections(*value)
		if !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			selected, _ = mergeSelectionLocks(global, *value)
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
	if value.SchemaRevision == lockSchemaRevision {
		return false, nil
	}
	if value.SchemaRevision == previousLockSchemaRevision {
		value.SchemaRevision = lockSchemaRevision
		return true, nil
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

func (m *manager) validatePersistedRemoteRef(expected remoteSkillRef) error {
	current, err := m.persistedRemoteRef(expected.key(), expected.Name)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("persisted remote skill identity changed")
	}
	return nil
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
	for name := range projectLock.Expressions {
		if err := add(name); err != nil {
			return false, err
		}
	}
	for name := range globalLock.Skills {
		if _, overridden := projectLock.enabled(name); overridden {
			continue
		}
		if err := add(name); err != nil {
			return false, err
		}
	}
	for name := range globalLock.Expressions {
		if _, overridden := projectLock.enabled(name); overridden {
			continue
		}
		if err := add(name); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func configuredSelections(value lock) map[string]bool {
	selected := maps.Clone(value.Skills)
	for name := range value.Expressions {
		selected[name] = true
	}
	return selected
}

func mergeSelectionLocks(global, project lock) (map[string]bool, map[string]string) {
	selected := configuredSelections(global)
	expressions := maps.Clone(global.Expressions)
	for name, enabled := range project.Skills {
		selected[name] = enabled
		delete(expressions, name)
	}
	for name, expression := range project.Expressions {
		selected[name] = true
		expressions[name] = expression
	}
	return selected, expressions
}

func skillEnabled(selected map[string]bool, skill discoveredSkill) bool {
	enabled, explicit := selected[skill.Name]
	if explicit {
		return enabled
	}
	return skill.Source == projectSkillSource || skill.DisableModelInvocation
}

type enabledEvaluator struct {
	project     string
	callHandler interp.CallHandlerFunc
}

func newEnabledEvaluator(project string) *enabledEvaluator {
	return &enabledEvaluator{
		project:     project,
		callHandler: enabledCallHandler(project),
	}
}

func (e *enabledEvaluator) evaluate(ctx context.Context, skill, expression string) (bool, error) {
	program, err := syntax.NewParser(
		syntax.Variant(syntax.LangBash),
	).Parse(strings.NewReader(expression), "enabled")
	if err != nil {
		return false, fmt.Errorf("parse enabled expression for skill %q: %w", skill, err)
	}
	runner, err := interp.New(
		interp.Dir(e.project),
		interp.StdIO(nil, io.Discard, io.Discard),
		interp.CallHandler(e.callHandler),
	)
	if err != nil {
		return false, fmt.Errorf("prepare enabled expression for skill %q: %w", skill, err)
	}
	err = runner.Run(ctx, program)
	if err == nil {
		return true, nil
	}
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		if status == 1 {
			return false, nil
		}
		return false, fmt.Errorf(
			"evaluate enabled expression for skill %q: status %d",
			skill,
			uint8(status),
		)
	}
	return false, fmt.Errorf("evaluate enabled expression for skill %q: %w", skill, err)
}

func sourceAgent(source string) string {
	switch source {
	case "codex", "admin", "bundled", "plugin":
		return "codex"
	case "claude":
		return "claude"
	case "grok":
		return "grok"
	default:
		return ""
	}
}

func harnessAgent(harness listHarness) string {
	switch harness {
	case listHarnessClaude:
		return "claude"
	case listHarnessGrok:
		return "grok"
	case listHarnessCodex:
		return "codex"
	default:
		return ""
	}
}

func skillVisibleInTUI(skill discoveredSkill, harnesses []listHarness) bool {
	agent := sourceAgent(skill.Source)
	if len(harnesses) == 0 {
		return agent == ""
	}
	if agent == "" {
		return false
	}
	return skillAllowedForAgent(skill, harnesses, false)
}

func skillAllowedForAgent(skill discoveredSkill, harnesses []listHarness, unscopedIncludesPrivate bool) bool {
	agent := sourceAgent(skill.Source)
	if agent == "" {
		return true
	}
	if len(harnesses) == 0 {
		return unscopedIncludesPrivate
	}
	for _, harness := range harnesses {
		if harnessAgent(harness) != agent {
			return false
		}
	}
	return true
}

func catalogSkills(skills []discoveredSkill, harnesses []listHarness) []discoveredSkill {
	filtered := make([]discoveredSkill, 0, len(skills))
	for _, skill := range skills {
		if skillVisibleInTUI(skill, harnesses) {
			filtered = append(filtered, skill)
		}
	}
	return filtered
}

type listedSkillXML struct {
	Name        string         `xml:"name,attr"`
	Description string         `xml:"description,attr"`
	References  *listedTextXML `xml:"references,omitempty"`
}

type listedTextXML struct {
	Content string `xml:",innerxml"`
}

type skillListXML struct {
	XMLName xml.Name         `xml:"skills"`
	Skills  []listedSkillXML `xml:"skill"`
}

type listHarness uint8

const (
	listHarnessClaude listHarness = iota
	listHarnessGrok
	listHarnessCodex
)

func (m *manager) listContext(
	ctx context.Context,
	project string,
	output io.Writer,
	harnesses ...listHarness,
) error {
	skills, err := m.skills(project, harnesses...)
	if err != nil {
		return err
	}
	selection, err := m.selectionState(project, skills)
	if err != nil {
		return err
	}
	evaluator := newEnabledEvaluator(project)
	harnessVisible, err := m.harnessVisibleSkillNames(project, harnesses)
	if err != nil {
		return err
	}

	document := skillListXML{}
	for _, skill := range skills {
		if skill.DisableModelInvocation ||
			harnessVisible[skill.Name] {
			continue
		}
		enabled, err := selection.enabled(ctx, evaluator, skill.Name)
		if err != nil {
			return err
		}
		if !enabled {
			continue
		}
		references, err := referenceFiles(skill.Root)
		if err != nil {
			return fmt.Errorf("%s: %w", skill.Name, err)
		}
		listed := listedSkillXML{
			Name:        skill.Name,
			Description: skill.Description,
		}
		if len(references) > 0 {
			content, err := formatReferenceList(references)
			if err != nil {
				return fmt.Errorf("format references for %s: %w", skill.Name, err)
			}
			listed.References = &listedTextXML{Content: content}
		}
		document.Skills = append(document.Skills, listed)
	}

	encoder := xml.NewEncoder(output)
	encoder.Indent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output)
	return err
}

func (m *manager) harnessVisibleSkillNames(project string, harnesses []listHarness) (map[string]bool, error) {
	discovery := skillDiscovery{
		seenPaths: make(map[string]struct{}),
		seenNames: make(map[string]struct{}),
	}
	for _, harness := range harnesses {
		var roots []skillRoot
		switch harness {
		case listHarnessClaude:
			roots = []skillRoot{
				{path: filepath.Join(project, ".claude", "skills")},
				{path: m.paths.claudeSkills},
			}
		case listHarnessGrok:
			roots = []skillRoot{
				{path: filepath.Join(project, ".grok", "skills")},
				{path: m.paths.grokSkills},
			}
		case listHarnessCodex:
			roots = []skillRoot{
				{path: filepath.Join(project, ".codex", "skills"), includeSystem: true},
				{path: filepath.Join(m.paths.codexHome, "skills"), includeSystem: true},
				{path: m.paths.adminSkills},
			}
		}
		// Grok loads shared .agents/skills roots natively. Claude does not.
		if harness == listHarnessGrok {
			roots = append(roots,
				skillRoot{path: filepath.Join(project, ".agents", "skills")},
				skillRoot{path: m.paths.userSkills},
			)
		}
		for _, root := range roots {
			if err := discovery.discoverRoot(root); err != nil {
				return nil, err
			}
		}
		if harness == listHarnessCodex {
			if err := discovery.discoverPluginCache(filepath.Join(m.paths.codexHome, "plugins", "cache")); err != nil {
				return nil, err
			}
		}
	}

	visible := make(map[string]bool, len(discovery.skills))
	for _, skill := range discovery.skills {
		visible[skill.Name] = true
	}
	return visible, nil
}

func formatReferenceList(files []string) (string, error) {
	lines := make([]string, 0, len(files)+1)
	var nested []string
	for _, file := range files {
		if relative, ok := strings.CutPrefix(file, "references/"); ok {
			nested = append(nested, "  "+relative)
			continue
		}
		lines = append(lines, file)
	}
	if len(nested) > 0 {
		lines = append(lines, "references/")
		lines = append(lines, nested...)
	}
	var output strings.Builder
	for index, line := range lines {
		if index > 0 {
			output.WriteByte('\n')
		}
		escaped, err := escapeXMLText(line)
		if err != nil {
			return "", err
		}
		output.WriteString(escaped)
	}
	return output.String(), nil
}

func escapeXMLText(value string) (string, error) {
	var output strings.Builder
	if err := xml.EscapeText(&output, []byte(value)); err != nil {
		return "", err
	}
	escaped := strings.ReplaceAll(output.String(), "&#34;", `"`)
	return strings.ReplaceAll(escaped, "&#39;", "'"), nil
}

func (m *manager) getContext(
	ctx context.Context,
	project, target, lineRange string,
	output io.Writer,
) error {
	skill, relative, err := splitTarget(target)
	if err != nil {
		return err
	}
	root, err := m.openSkillContext(ctx, project, skill)
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

func (m *manager) scriptCommandContext(
	ctx context.Context,
	project, target string,
	args []string,
) (*exec.Cmd, error) {
	if !strings.Contains(target, "/") {
		return nil, fmt.Errorf("invalid script target %q; want <skill-name>/<relative/script>", target)
	}
	skill, relative, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	root, err := m.openSkillContext(ctx, project, skill)
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

func (m *manager) openSkillContext(
	ctx context.Context,
	project, skill string,
) (*os.Root, error) {
	discovered, err := m.findSkill(project, skill)
	if err != nil {
		return nil, err
	}
	selection, err := m.selectionState(project, []discoveredSkill{discovered})
	if err != nil {
		return nil, err
	}
	enabled, err := selection.enabled(ctx, newEnabledEvaluator(project), skill)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, fmt.Errorf("skill %q is not enabled", skill)
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

func (m *manager) toggleModelInvocation(
	ctx context.Context,
	project string,
	expected discoveredSkill,
) (modelInvocationResult, error) {
	current, err := m.findSkill(project, expected.Name)
	if err != nil {
		return modelInvocationResult{}, err
	}
	if current.Path != expected.Path || current.RemoteKey != expected.RemoteKey {
		return modelInvocationResult{}, fmt.Errorf(
			"skill %q source changed before model invocation was updated",
			expected.Name,
		)
	}

	var disabled bool
	if current.RemoteKey != "" {
		ref, err := m.persistedRemoteRef(current.RemoteKey, current.Name)
		if err != nil {
			return modelInvocationResult{}, err
		}
		disabled, err = m.remoteStore.toggleModelInvocation(ctx, ref)
		if err != nil {
			return modelInvocationResult{}, err
		}
	} else {
		if !current.Editable {
			return modelInvocationResult{}, fmt.Errorf(
				"skill %q is not editable at its discovered source",
				current.Name,
			)
		}
		disabled, err = toggleModelInvocationFile(
			ctx,
			m.paths.selectionLocks,
			current.Path,
		)
		if err != nil {
			return modelInvocationResult{}, err
		}
	}

	skills, err := m.skills(project)
	if err != nil {
		return modelInvocationResult{}, err
	}
	selection, err := m.selectionState(project, skills)
	if err != nil {
		return modelInvocationResult{}, err
	}
	return modelInvocationResult{
		Skill:           current.Name,
		RemoteKey:       current.RemoteKey,
		Disabled:        disabled,
		Skills:          skills,
		Selected:        selection.selected,
		GlobalSelected:  selection.globalSelected,
		ProjectSelected: selection.projectSelected,
	}, nil
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

func listedReferenceName(name string) bool {
	return !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

func referenceFiles(skillRoot string) ([]string, error) {
	entries, err := os.ReadDir(skillRoot)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() ||
			entry.Name() == "SKILL.md" ||
			filepath.Ext(entry.Name()) != ".md" ||
			!listedReferenceName(entry.Name()) {
			continue
		}
		files = append(files, entry.Name())
	}

	referencesRoot := filepath.Join(skillRoot, "references")
	err = filepath.WalkDir(referencesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != referencesRoot && !listedReferenceName(entry.Name()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
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
