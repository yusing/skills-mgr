package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type manager struct {
	paths paths
}

func (m *manager) skills() ([]string, error) {
	entries, err := os.ReadDir(m.paths.library)
	if err != nil {
		return nil, fmt.Errorf("read skill library: %w", err)
	}
	var names []string
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(m.paths.library, entry.Name(), "SKILL.md"))
		if err == nil && info.Mode().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
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
	names, err := m.skills()
	if err != nil {
		return err
	}
	available := make(map[string]bool, len(names))
	for _, name := range names {
		available[name] = true
	}
	for name, enabled := range selected {
		if enabled && !available[name] {
			return fmt.Errorf("enabled skill %q is missing from the library", name)
		}
	}
	for _, name := range names {
		if !selected[name] {
			continue
		}
		root := filepath.Join(m.paths.library, name)
		description, err := skillDescription(filepath.Join(root, "SKILL.md"))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		references, err := referenceFiles(root)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := fmt.Fprintf(output, "%s — %s\n", name, description); err != nil {
			return err
		}
		if len(references) > 0 {
			if _, err := fmt.Fprintln(output, "└── references/"); err != nil {
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
	selected, err := m.selection(project)
	if err != nil {
		return err
	}
	if !selected[skill] {
		return fmt.Errorf("skill %q is not enabled", skill)
	}
	rootPath, err := filepath.EvalSymlinks(filepath.Join(m.paths.library, skill))
	if err != nil {
		return fmt.Errorf("resolve skill %q: %w", skill, err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open skill %q: %w", skill, err)
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

func skillDescription(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read metadata: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", fmt.Errorf("missing front matter")
	}
	for index := 1; index < len(lines); index++ {
		line := lines[index]
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "description" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, ">") || strings.HasPrefix(value, "|") {
			var parts []string
			for index++; index < len(lines); index++ {
				if strings.TrimSpace(lines[index]) == "---" || len(lines[index]) == 0 || lines[index][0] != ' ' && lines[index][0] != '\t' {
					index--
					break
				}
				if part := strings.TrimSpace(lines[index]); part != "" {
					parts = append(parts, part)
				}
			}
			value = strings.Join(parts, " ")
		} else {
			value, err = yamlScalar(value)
			if err != nil {
				return "", fmt.Errorf("decode description: %w", err)
			}
		}
		if value == "" {
			return "", fmt.Errorf("empty description")
		}
		return value, nil
	}
	return "", fmt.Errorf("missing description")
}

func yamlScalar(value string) (string, error) {
	if strings.HasPrefix(value, `"`) {
		return strconv.Unquote(value)
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	return value, nil
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
		for _, part := range strings.Split(file, "/") {
			if node.children[part] == nil {
				node.children[part] = &referenceNode{children: make(map[string]*referenceNode)}
			}
			node = node.children[part]
		}
	}
	return writeReferenceNodes(output, root, "    ")
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
