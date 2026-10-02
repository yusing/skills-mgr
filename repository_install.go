package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

type repositoryLocation struct {
	cloneURL string
	locator  string
	ref      string
	path     string
}

// Prefetched files let the initial install reuse its discovery checkout.
// Refresh and sync use the same provider without prefetched content.
type repositoryContentProvider struct {
	files []remoteSkillFile
}

func (p repositoryContentProvider) fetchSkill(ctx context.Context, ref remoteSkillRef) ([]remoteSkillFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.Provider != repositoryProvider {
		return nil, fmt.Errorf("invalid repository skill reference")
	}
	if p.files != nil {
		return p.files, nil
	}
	resolved, files, err := loadRepositorySkill(ctx, ref.Locator, ref.Name, io.Discard)
	if err != nil {
		return nil, err
	}
	if resolved != ref {
		return nil, fmt.Errorf("repository skill identity changed")
	}
	return files, nil
}

func (m *manager) installRepository(ctx context.Context, project, address, name string, output, progress io.Writer) error {
	ref, files, err := loadRepositorySkill(ctx, address, name, progress)
	if err != nil {
		return err
	}
	fmt.Fprintf(progress, "Installing and enabling %s...\n", ref.Name)
	if m.global {
		if err := os.MkdirAll(m.paths.globalLockDir, 0o755); err != nil {
			return fmt.Errorf("create global selection directory: %w", err)
		}
	}
	if _, err := m.selectRemote(ctx, project, ref, repositoryContentProvider{files: files}, false); err != nil {
		return err
	}
	layer := "project"
	if m.global {
		layer = "global"
	}
	_, err = fmt.Fprintf(output, "Installed %s (%s selection)\n", ref.Name, layer)
	return err
}

func parseRepositoryLocation(address string) (repositoryLocation, error) {
	address = strings.TrimSpace(address)
	if strings.HasPrefix(address, "github.com/") {
		address = "https://" + address
	} else if !strings.Contains(address, ":") && strings.Count(address, "/") == 1 {
		address = "https://github.com/" + address
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return repositoryLocation{}, fmt.Errorf("expected an HTTPS Git repository address or GitHub owner/repo")
	}
	for _, char := range address {
		if unicode.IsControl(char) {
			return repositoryLocation{}, fmt.Errorf("repository address contains control characters")
		}
	}
	rawParts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	parts := make([]string, len(rawParts))
	github := strings.EqualFold(parsed.Host, "github.com")
	for index, raw := range rawParts {
		part, err := url.PathUnescape(raw)
		// An encoded slash is useful in GitHub branch names, but never in paths.
		if err != nil || part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") ||
			strings.Contains(part, "/") && !(github && index == 3) {
			return repositoryLocation{}, fmt.Errorf("repository address has an invalid path")
		}
		for _, char := range part {
			if unicode.IsControl(char) {
				return repositoryLocation{}, fmt.Errorf("repository address contains control characters")
			}
		}
		parts[index] = part
	}
	if !github {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
		parsed.RawPath = strings.TrimSuffix(parsed.RawPath, "/")
		return repositoryLocation{cloneURL: parsed.String(), locator: parsed.String()}, nil
	}
	if len(parts) < 2 {
		return repositoryLocation{}, fmt.Errorf("GitHub address must include an owner and repository")
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if !validGitHubRepositoryPart(parts[0]) || !validGitHubRepositoryPart(repo) {
		return repositoryLocation{}, fmt.Errorf("invalid GitHub owner or repository")
	}
	base := "https://github.com/" + parts[0] + "/" + repo
	location := repositoryLocation{cloneURL: base + ".git", locator: base}
	if len(parts) == 2 {
		return location, nil
	}
	if len(parts) < 4 || parts[2] != "tree" && parts[2] != "blob" || strings.HasPrefix(parts[3], "-") {
		return repositoryLocation{}, fmt.Errorf("expected a GitHub repository root, /tree/<ref>/<path>, or /blob/<ref>/<path>/SKILL.md")
	}
	if parts[2] == "blob" {
		if len(parts) < 5 || parts[len(parts)-1] != skillManifestName {
			return repositoryLocation{}, fmt.Errorf("GitHub file address must point to SKILL.md")
		}
		parts = parts[:len(parts)-1]
	}
	location.ref = parts[3]
	location.path = strings.Join(parts[4:], "/")
	escaped := make([]string, len(parts)-3)
	for index, part := range parts[3:] {
		escaped[index] = url.PathEscape(part)
	}
	location.locator = base + "/tree/" + strings.Join(escaped, "/")
	return location, nil
}

func loadRepositorySkill(ctx context.Context, address, name string, progress io.Writer) (remoteSkillRef, []remoteSkillFile, error) {
	if err := ctx.Err(); err != nil {
		return remoteSkillRef{}, nil, err
	}
	if name != "" && !validSkillName(name) {
		return remoteSkillRef{}, nil, fmt.Errorf("invalid skill name %q", name)
	}
	location, err := parseRepositoryLocation(address)
	if err != nil {
		return remoteSkillRef{}, nil, err
	}
	fmt.Fprintln(progress, "Cloning skill repository...")
	temporary, err := os.MkdirTemp("", "skills-mgr-install-")
	if err != nil {
		return remoteSkillRef{}, nil, err
	}
	defer os.RemoveAll(temporary)
	checkout := filepath.Join(temporary, "repository")
	if err := cloneSkillRepository(ctx, location.cloneURL, location.ref, checkout); err != nil {
		return remoteSkillRef{}, nil, err
	}
	tracked, err := gitTrackedFiles(ctx, checkout)
	if err != nil {
		return remoteSkillRef{}, nil, err
	}
	fmt.Fprintln(progress, "Discovering repository skills...")
	var candidates []string
	for _, file := range tracked {
		if filepath.Base(file) == skillManifestName &&
			(location.path == "" || strings.HasPrefix(file, location.path+"/")) {
			candidates = append(candidates, file)
		}
	}
	// A supplied directory with its own manifest is a skill, not a collection.
	direct := skillManifestName
	if location.path != "" {
		direct = location.path + "/" + skillManifestName
	}
	if name == "" && slices.Contains(candidates, direct) {
		candidates = []string{direct}
	}
	if len(candidates) > remoteSkillMaxFiles {
		return remoteSkillRef{}, nil, fmt.Errorf("repository contains more than %d skill manifests", remoteSkillMaxFiles)
	}
	var matches []discoveredSkill
	var choices []string
	total := int64(0)
	for _, file := range candidates {
		if err := ctx.Err(); err != nil {
			return remoteSkillRef{}, nil, err
		}
		if _, err := validRemoteFilePath(file); err != nil {
			return remoteSkillRef{}, nil, err
		}
		manifest := filepath.Join(checkout, filepath.FromSlash(file))
		info, err := os.Lstat(manifest)
		if err != nil {
			return remoteSkillRef{}, nil, err
		}
		if !info.Mode().IsRegular() {
			return remoteSkillRef{}, nil, fmt.Errorf("repository skill manifest is not a regular file")
		}
		total += info.Size()
		if total > remoteSkillMaxBytes {
			return remoteSkillRef{}, nil, fmt.Errorf("repository skill manifests exceed %d bytes", remoteSkillMaxBytes)
		}
		skill, ok, err := parseSkill(manifest)
		if err != nil {
			return remoteSkillRef{}, nil, err
		}
		if !ok {
			continue
		}
		skill.Path = file
		choices = append(choices, skill.Name+" ("+filepath.ToSlash(filepath.Dir(file))+")")
		if name == "" || skill.Name == name {
			matches = append(matches, skill)
			if file == direct {
				matches = []discoveredSkill{skill}
				break
			}
		}
	}
	slices.Sort(choices)
	if len(matches) != 1 {
		return remoteSkillRef{}, nil, fmt.Errorf("repository has %d matching skills; supply a skill name or a skill directory URL; available: %s", len(matches), strings.Join(choices, ", "))
	}
	skill := matches[0]
	skillPath := filepath.ToSlash(filepath.Dir(skill.Path))
	if skillPath == "." {
		skillPath = ""
	}
	ref := remoteSkillRef{Provider: repositoryProvider, ID: location.locator + "#" + skill.Name, Name: skill.Name, Locator: location.locator}
	if err := ref.validate(); err != nil {
		return remoteSkillRef{}, nil, err
	}
	files, err := filesFromGitHubCheckout(ctx, checkout, skillPath, tracked)
	return ref, files, err
}
