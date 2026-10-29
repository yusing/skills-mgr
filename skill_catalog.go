package main

import (
	"context"
	"errors"
	"io/fs"

	"encoding/xml"

	"fmt"
	"io"

	"os"

	"path"
	"path/filepath"
	"slices"

	"strings"
)

func sourceAgent(source string) string {
	switch source {
	case "codex", "admin", "bundled", "plugin":
		return "codex"
	case "claude", "claude-plugin":
		return "claude"
	case "grok", "grok-plugin", "grok-bundled":
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
	return skillAllowedForAgent(skill, harnesses)
}

func skillAllowedForAgent(skill discoveredSkill, harnesses []listHarness) bool {
	agent := sourceAgent(skill.Source)
	if agent == "" || len(harnesses) == 0 {
		return true
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
	if selection.usesProjectEvidence() {
		// Preparation is speculative because shell control flow may skip every
		// literal builtin. The builtin surfaces any load error if it is reached.
		_ = evaluator.evidence.prepare(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	servesClaude := false
	if slices.Contains(harnesses, listHarnessClaude) {
		if servesClaude, err = m.claudeSkillToolDenied(project); err != nil {
			return err
		}
	}
	visibleHarnesses := harnesses
	if servesClaude {
		visibleHarnesses = slices.DeleteFunc(slices.Clone(harnesses), func(harness listHarness) bool {
			return harness == listHarnessClaude
		})
	}
	harnessVisible, err := m.harnessVisibleSkillNames(project, visibleHarnesses)
	if err != nil {
		return err
	}

	document := skillListXML{}
	listedNames := make(map[string]bool)
	add := func(skill discoveredSkill) error {
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
		listedNames[skill.Name] = true
		return nil
	}
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
		if err := add(skill); err != nil {
			return err
		}
	}
	if servesClaude {
		// Claude's enabled plugins decide these, as they do for get.
		plugins, err := m.claudePluginSkills()
		if err != nil {
			return err
		}
		for _, skill := range plugins {
			if !skill.ExternalEnabled || skill.DisableModelInvocation ||
				listedNames[skill.Name] || !skillAllowedForAgent(skill, harnesses) {
				continue
			}
			if err := add(skill); err != nil {
				return err
			}
		}
	}

	encoder := xml.NewEncoder(output)
	encoder.Indent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output)
	return err
}

// grokLoadsClaudeSkills reports whether Grok's Claude-compatibility scanning is
// on, which decides whether Grok already sees the .claude/skills roots. Grok
// scans them by default and documents GROK_CLAUDE_SKILLS_ENABLED as the way to
// stop it. Turning the same thing off through [compat.claude] in Grok's
// config.toml is not read here, so a root hidden that way is reported to the
// agent twice rather than withheld from it.
func grokLoadsClaudeSkills() bool {
	return !strings.EqualFold(os.Getenv("GROK_CLAUDE_SKILLS_ENABLED"), "false")
}

func (m *manager) harnessVisibleSkillNames(project string, harnesses []listHarness) (map[string]bool, error) {
	discovery := newSkillDiscovery()
	for _, harness := range harnesses {
		var roots []skillRoot
		switch harness {
		case listHarnessClaude:
			roots = []skillRoot{
				{path: m.paths.projectSkills(project, ".claude")},
				{path: m.paths.claudeSkills},
			}
		case listHarnessGrok:
			roots = []skillRoot{
				{path: m.paths.projectSkills(project, ".grok")},
				{path: m.paths.grokSkills},
				// Grok scans the shared .agents/skills roots natively. Claude
				// does not.
				{path: m.paths.projectSkills(project, ".agents")},
				{path: m.paths.userSkills},
			}
			if grokLoadsClaudeSkills() {
				roots = append(roots,
					skillRoot{path: m.paths.projectSkills(project, ".claude")},
					skillRoot{path: m.paths.claudeSkills},
				)
			}
		case listHarnessCodex:
			roots = []skillRoot{
				{path: m.paths.projectSkills(project, ".codex"), includeSystem: true},
				{path: m.paths.codexSkills(), includeSystem: true},
				{path: m.paths.adminSkills},
				{path: m.paths.codexPluginCache(), pluginCache: true},
			}
		}
		for _, root := range roots {
			if err := discovery.discover(root); err != nil {
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
			entry.Name() == skillManifestName ||
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

func formatReferenceList(files []string) (string, error) {
	var directories []string
	groups := make(map[string][]string)
	for _, file := range files {
		dir := path.Dir(file)
		if _, ok := groups[dir]; !ok {
			directories = append(directories, dir)
		}
		groups[dir] = append(groups[dir], strings.TrimSuffix(path.Base(file), ".md"))
	}
	quote := func(s string) string {
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_./-", c)) {
				return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
			}
		}
		return s
	}
	var parts []string
	for _, dir := range directories {
		var prefix string
		if dir != "." {
			prefix = quote(dir) + "/"
		}
		names := groups[dir]
		for i := range names {
			names[i] = quote(names[i])
		}
		if len(names) == 1 {
			parts = append(parts, prefix+names[0]+".md")
		} else {
			parts = append(parts, prefix+"{"+strings.Join(names, ",")+"}.md")
		}
	}
	return escapeXMLText(strings.Join(parts, " "))
}

func escapeXMLText(value string) (string, error) {
	var output strings.Builder
	if err := xml.EscapeText(&output, []byte(value)); err != nil {
		return "", err
	}
	escaped := strings.ReplaceAll(output.String(), "&#34;", `"`)
	return strings.ReplaceAll(escaped, "&#39;", "'"), nil
}
