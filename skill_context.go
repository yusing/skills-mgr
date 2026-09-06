package main

import (
	"bufio"
	"bytes"
	"context"

	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"strconv"
	"strings"
)

type skillLocationResult struct {
	Skill           string
	Managed         bool
	Skills          []discoveredSkill
	Selected        map[string]bool
	GlobalSelected  map[string]bool
	ProjectSelected map[string]bool
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

func (m *manager) getContext(
	ctx context.Context,
	project, target, lineRange string,
	output io.Writer,
) error {
	skill, relative, err := splitTarget(target)
	if err != nil {
		return err
	}
	root, discovered, err := m.openSkillContext(ctx, project, skill)
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
	var patchErr error
	isSkillMarkdown := filepath.Clean(filepath.FromSlash(relative)) == skillManifestName
	if discovered.RemoteKey != "" && isSkillMarkdown {
		original, err := io.ReadAll(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", target, err)
		}
		ref, err := m.persistedRemoteRef(discovered.RemoteKey, discovered.Name)
		if err == nil {
			var patched []byte
			patched, err = m.remoteStore.applyPatch(ref, original)
			if err == nil {
				input = bytes.NewReader(patched)
			}
		}
		if err != nil {
			patchErr = err
			input = bytes.NewReader(original)
		}
	}
	if strings.EqualFold(filepath.Ext(relative), ".md") {
		_, body, status, err := readFrontmatter(input)
		if err != nil {
			return fmt.Errorf("read %s frontmatter: %w", target, err)
		}
		switch status {
		case frontmatterValid:
			input = body
		case frontmatterAbsent:
			if isSkillMarkdown {
				return fmt.Errorf("%s has invalid frontmatter", target)
			}
			input = body
		case frontmatterMalformed:
			return fmt.Errorf("%s has invalid frontmatter", target)
		}
	}
	if lineRange == "" {
		_, err := io.Copy(output, input)
		return errors.Join(err, patchErr)
	}
	return errors.Join(writeLineRange(output, input, start, end), patchErr)
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
	root, _, err := m.openSkillContext(ctx, project, skill)
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
) (*os.Root, discoveredSkill, error) {
	discovered, err := m.findAccessibleSkill(ctx, project, skill)
	if err != nil {
		return nil, discoveredSkill{}, err
	}
	root, err := os.OpenRoot(discovered.Root)
	if err != nil {
		return nil, discoveredSkill{}, fmt.Errorf("open skill %q: %w", skill, err)
	}
	return root, discovered, nil
}

// findAccessibleSkill returns the first enabled skill of this name, searching
// filesystem discovery, then Claude plugins, then Grok native skills. A catalog
// that fails to load is skipped when another catalog can serve the name; its
// error is returned only when the name is absent from every catalog that loaded.
func (m *manager) findAccessibleSkill(
	ctx context.Context,
	project, name string,
) (discoveredSkill, error) {
	inaccessible := false
	try := func(skills []discoveredSkill) (discoveredSkill, bool, error) {
		for _, discovered := range skills {
			if discovered.Name != name {
				continue
			}
			enabled, err := m.skillAccessible(ctx, project, discovered)
			if err != nil {
				return discoveredSkill{}, false, err
			}
			if enabled {
				return discovered, true, nil
			}
			inaccessible = true
		}
		return discoveredSkill{}, false, nil
	}

	skills, err := m.skills(project)
	if err != nil {
		return discoveredSkill{}, err
	}
	if discovered, ok, err := try(skills); err != nil || ok {
		return discovered, err
	}

	var catalogErr error
	claude, err := m.claudePluginSkills()
	if err != nil {
		catalogErr = err
	} else if discovered, ok, err := try(claude); err != nil || ok {
		return discovered, err
	}

	grok, err := m.grokNativeSkills(project)
	if err != nil {
		if catalogErr == nil {
			catalogErr = err
		}
	} else if discovered, ok, err := try(grok); err != nil || ok {
		return discovered, err
	}

	if inaccessible {
		return discoveredSkill{}, fmt.Errorf("skill %q is not enabled", name)
	}
	if catalogErr != nil {
		return discoveredSkill{}, catalogErr
	}
	return discoveredSkill{}, fmt.Errorf("skill %q was not discovered", name)
}

func (m *manager) skillAccessible(
	ctx context.Context,
	project string,
	discovered discoveredSkill,
) (bool, error) {
	if isNativeSkill(discovered) {
		return discovered.ExternalEnabled, nil
	}
	selection, err := m.selectionState(project, []discoveredSkill{discovered})
	if err != nil {
		return false, err
	}
	return selection.enabled(ctx, newEnabledEvaluator(project), discovered.Name)
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
		return skill, skillManifestName, nil
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
