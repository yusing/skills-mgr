package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type inspectedSelection struct {
	Layer string        `json:"layer"`
	Path  string        `json:"path,omitempty"`
	Value *enabledValue `json:"value,omitempty"`
}

type inspectedSkill struct {
	Source                 string             `json:"source"`
	Scope                  string             `json:"scope"`
	Root                   string             `json:"root"`
	Path                   string             `json:"path"`
	Plugin                 string             `json:"plugin,omitempty"`
	DisableModelInvocation bool               `json:"disable_model_invocation"`
	Editable               bool               `json:"editable"`
	Enabled                bool               `json:"enabled"`
	Selection              inspectedSelection `json:"selection"`
	RemoteKey              string             `json:"remote_key,omitempty"`
	ContentMissing         bool               `json:"content_missing,omitempty"`
	PatchPath              string             `json:"patch_path,omitempty"`
	SHA256                 string             `json:"sha256,omitempty"`
	BodyHealth             string             `json:"body_health"`
	References             []string           `json:"references"`
	Error                  string             `json:"error,omitempty"`
}

type skillInspection struct {
	Name       string           `json:"name"`
	Project    string           `json:"project"`
	Resolved   *inspectedSkill  `json:"resolved"`
	Fallback   bool             `json:"fallback"`
	Candidates []inspectedSkill `json:"candidates"`
	Warnings   []string         `json:"warnings,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// inspectContext retains the same filesystem-first, Claude-then-Grok precedence
// as get. Native alternatives are included even when the filesystem owner wins.
func (m *manager) inspectContext(ctx context.Context, project, name string, out io.Writer) error {
	if !validSkillName(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	report := skillInspection{Name: name, Project: project, Candidates: []inspectedSkill{}}
	skills, err := m.discoverSkillsNamed(project, "", name)
	if err != nil {
		return err
	}
	// Discovery intentionally omits missing caches. Inspection keeps their
	// persisted identity visible, without treating them as accessible content.
	if len(skills) == 0 && m.remoteStore != nil {
		records, err := m.remoteStore.recordsNamed(name)
		if err != nil {
			return err
		}
		for _, record := range records {
			if _, err := m.remoteStore.contentRoot(record); errors.Is(err, errRemoteSkillContentMissing) {
				root := filepath.Join(m.remoteStore.root, record.Content)
				skills = append(skills, discoveredSkill{Name: name, Source: record.Provider, Root: root, Path: filepath.Join(root, skillManifestName), RemoteKey: record.ref().key(), Editable: true, ContentMissing: true})
			} else if err != nil {
				return err
			}
		}
	}
	var catalogErr error
	for _, load := range []func() ([]discoveredSkill, error){m.claudePluginSkills, func() ([]discoveredSkill, error) { return m.grokNativeSkills(project) }} {
		catalog, err := load()
		if err != nil {
			report.Warnings = append(report.Warnings, err.Error())
			if catalogErr == nil {
				catalogErr = err
			}
			continue
		}
		for _, skill := range catalog {
			if skill.Name == name {
				skills = append(skills, skill)
			}
		}
	}
	var accessErr error
	evaluator := newEnabledEvaluator(project)
	atHome, err := m.paths.atHome(project)
	if err != nil {
		return err
	}
	for _, skill := range skills {
		scope := "shared"
		if isNativeSkill(skill) {
			scope = "native"
		} else if skill.Source == projectSkillSource {
			scope = "project"
		} else if !atHome && skill.RemoteKey == "" && skill.Source != managedSkillSource && skill.Source != "user" {
			if relative, err := filepath.Rel(project, skill.Root); err == nil && filepath.IsLocal(relative) {
				scope = "project"
			}
		}
		item := inspectedSkill{Source: skill.Source, Scope: scope, Root: skill.Root, Path: skill.Path, Plugin: skill.Plugin, DisableModelInvocation: skill.DisableModelInvocation, Editable: skill.Editable, RemoteKey: skill.RemoteKey, ContentMissing: skill.ContentMissing, References: []string{}}
		item.Selection, err = m.inspectSelection(project, skill)
		if err != nil {
			return err
		}
		if value := item.Selection.Value; value.Boolean != nil {
			item.Enabled = *value.Boolean
			err = nil
		} else {
			item.Enabled, err = evaluator.evaluate(ctx, name, item.Selection.Value.Expression)
		}
		if err != nil {
			item.Error = err.Error()
			if report.Resolved == nil && accessErr == nil && !skill.ContentMissing {
				accessErr = err
			}
		}
		if skill.RemoteKey != "" {
			ref, refErr := m.persistedRemoteRef(skill.RemoteKey, skill.Name)
			if refErr != nil {
				return refErr
			}
			item.PatchPath = m.remoteStore.patchPath(ref)
		}
		contents, bodyErr := m.skillManifest(skill)
		if bodyErr == nil {
			item.SHA256 = fmt.Sprintf("%x", sha256.Sum256(contents))
			bodyErr = validateResourceBody(contents)
		}
		item.BodyHealth = "ok"
		if bodyErr != nil {
			item.BodyHealth = "error"
			item.Error = errors.Join(err, bodyErr).Error()
		}
		refs, refErr := referenceFiles(skill.Root)
		if refErr != nil {
			if !skill.ContentMissing {
				report.Warnings = append(report.Warnings, refErr.Error())
			}
		} else if refs != nil {
			item.References = refs
		}
		report.Candidates = append(report.Candidates, item)
		if report.Resolved == nil && accessErr == nil && item.Enabled && !skill.ContentMissing {
			resolved := item
			report.Resolved = &resolved
			report.Fallback = isNativeSkill(skill) && len(report.Candidates) > 1
		}
	}
	if report.Resolved == nil {
		switch {
		case accessErr != nil:
			err = accessErr
		case len(skills) != 0 && skills[0].ContentMissing:
			err = fmt.Errorf("skill %q has missing cached content; run skills-mgr sync", name)
		case len(skills) != 0:
			err = fmt.Errorf("skill %q is not enabled", name)
		case catalogErr != nil:
			err = catalogErr
		default:
			err = fmt.Errorf("skill %q was not discovered", name)
		}
		report.Error = err.Error()
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if writeErr := encoder.Encode(report); writeErr != nil {
		return writeErr
	}
	// A disabled owner is a successful inspection, not a failed acquisition.
	if len(skills) == 0 {
		return err
	}
	return accessErr
}

func (m *manager) inspectSelection(project string, skill discoveredSkill) (inspectedSelection, error) {
	if isNativeSkill(skill) {
		return inspectedSelection{Layer: "native", Value: &enabledValue{Boolean: new(skill.ExternalEnabled)}}, nil
	}
	for _, layer := range []struct{ name, dir string }{{"project", project}, {"global", m.paths.globalLockDir}} {
		value, err := loadLock(layer.dir)
		if err != nil {
			return inspectedSelection{}, err
		}
		if enabled, exists := value.enabled(skill.Name); exists {
			return inspectedSelection{Layer: layer.name, Path: filepath.Join(layer.dir, lockName), Value: &enabled}, nil
		}
	}
	return inspectedSelection{Layer: "default", Value: &enabledValue{Boolean: new(skillEnabled(nil, skill))}}, nil
}

// skillManifest reads the owning file, including frontmatter and a remote patch.
// Unlike layeredContent, a stale patch is a health failure, not a clean fallback.
func (m *manager) skillManifest(skill discoveredSkill) ([]byte, error) {
	info, err := os.Stat(skill.Path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", skill.Path)
	}
	data, err := os.ReadFile(skill.Path)
	if err != nil || skill.RemoteKey == "" {
		return data, err
	}
	ref, err := m.persistedRemoteRef(skill.RemoteKey, skill.Name)
	if err != nil {
		return nil, err
	}
	return m.remoteStore.applyPatch(ref, data)
}

func validateResourceBody(data []byte) error {
	_, body, status, err := readFrontmatter(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if status == frontmatterMalformed {
		return fmt.Errorf("invalid frontmatter")
	}
	contents, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return fmt.Errorf("missing body or frontmatter-only placeholder")
	}
	return nil
}

func (m *manager) checkContext(ctx context.Context, project string, targets []string, out, diagnostics io.Writer) error {
	var failures error
	seen := make(map[string]bool)
	type resolution struct {
		skill discoveredSkill
		err   error
	}
	resolved := make(map[string]resolution)
	resolve := func(name string) resolution {
		if result, exists := resolved[name]; exists {
			return result
		}
		skill, err := m.findAccessibleSkill(ctx, project, name)
		result := resolution{skill, err}
		resolved[name] = result
		return result
	}
	check := func(target string) error {
		if seen[target] {
			return nil
		}
		seen[target] = true
		var body bytes.Buffer
		name, relative, err := splitTarget(target)
		var skill discoveredSkill
		if err == nil {
			result := resolve(name)
			skill, err = result.skill, result.err
		}
		if err == nil {
			err = m.writeSkillFile(skill, target, relative, "", &body)
		}
		if err == nil {
			// get already strips Markdown frontmatter, except the small metadata
			// header retained for user-invoked skill manifests. Do not parse a
			// legitimate leading Markdown horizontal rule a second time.
			if isSkillManifest(relative) && skill.DisableModelInvocation {
				err = validateResourceBody(body.Bytes())
			}
			if err == nil && len(bytes.TrimSpace(body.Bytes())) == 0 {
				err = fmt.Errorf("missing body or frontmatter-only placeholder")
			}
		}
		if err != nil {
			failure := fmt.Errorf("%s: %w", target, err)
			if _, writeErr := fmt.Fprintln(diagnostics, "FAIL", failure); writeErr != nil {
				return writeErr
			}
			failures = errors.Join(failures, failure)
			return nil
		}
		_, err = fmt.Fprintln(out, "PASS", target)
		return err
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := check(target); err != nil {
			return err
		}
		name, relative, err := splitTarget(target)
		if err != nil || !isSkillManifest(relative) {
			continue
		}
		result := resolve(name)
		if result.err != nil {
			continue
		}
		refs, err := referenceFiles(result.skill.Root)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("%s references: %w", name, err))
			continue
		}
		for _, ref := range refs {
			if err := check(name + "/" + ref); err != nil {
				return err
			}
		}
	}
	return failures
}
