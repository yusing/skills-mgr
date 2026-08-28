package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	skillsMPURL            = "https://skillsmp.com"
	skillsMPSchemaRevision = 1
	skillsMPMaxSearches    = 16
	skillsMPCacheTTL       = 5 * time.Minute
)

type skillsMPCache struct {
	SchemaRevision int                             `json:"schemaRevision"`
	UpdatedAt      time.Time                       `json:"updatedAt"`
	Skills         []skillsMPSkill                 `json:"skills"`
	Searches       map[string]skillsMPSearchResult `json:"searches,omitempty"`
}

type skillsMPSearchResult struct {
	UpdatedAt time.Time       `json:"updatedAt"`
	Skills    []skillsMPSkill `json:"skills"`
}

type skillsMPSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Author      string `json:"author"`
	Description string `json:"description"`
	GitHubURL   string `json:"githubUrl"`
	Stars       int    `json:"stars"`
}

type skillsMPRegistry struct {
	baseURL         string
	cachePath       string
	searchCachePath string
	apiKey          string
	client          *http.Client
	mu              sync.Mutex
}

func newSkillsMPRegistry(cachePath, apiKey string) *skillsMPRegistry {
	searchCachePath := ""
	if cachePath != "" {
		searchCachePath = filepath.Join(filepath.Dir(cachePath), "skillsmp-search.json")
	}
	return &skillsMPRegistry{
		baseURL:         skillsMPURL,
		cachePath:       cachePath,
		searchCachePath: searchCachePath,
		apiKey:          apiKey,
		client:          &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *skillsMPRegistry) catalog(ctx context.Context) ([]skillsMPSkill, error) {
	skills, err := r.fetch(ctx, "/api/skills", url.Values{
		"page":   {"1"},
		"limit":  {"200"},
		"sortBy": {"stars"},
	}, false)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	err = saveSkillsMPCache(r.cachePath, skillsMPCache{
		SchemaRevision: skillsMPSchemaRevision,
		UpdatedAt:      time.Now().UTC(),
		Skills:         skills,
	})
	r.mu.Unlock()
	return skills, err
}

func (r *skillsMPRegistry) search(
	ctx context.Context,
	query string,
) ([]registrySearchSkill, error) {
	skills, err := r.searchSkills(ctx, query)
	return presentSkillsMP(skills), err
}

func presentSkillsMP(skills []skillsMPSkill) []registrySearchSkill {
	results := make([]registrySearchSkill, len(skills))
	for index, skill := range skills {
		results[index] = registrySearchSkill{
			ID:          skill.ID,
			Name:        skill.Name,
			Description: skill.Description,
			Label:       fmt.Sprintf("%s • %d stars", skill.Author, skill.Stars),
			Provider:    skillsMPProvider,
			Locator:     skill.GitHubURL,
		}
	}
	return results
}

type githubSkillLocation struct {
	owner     string
	repo      string
	treeParts []string
}

func (r *skillsMPRegistry) fetchSkill(
	ctx context.Context,
	ref remoteSkillRef,
) ([]remoteSkillFile, error) {
	if ref.Provider != skillsMPProvider || ref.Locator == "" {
		return nil, fmt.Errorf("invalid SkillsMP skill reference")
	}
	location, err := parseGitHubSkillLocation(ref.Locator)
	if err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("", "skills-mgr-clone-")
	if err != nil {
		return nil, fmt.Errorf("create clone directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	checkout := filepath.Join(temporary, "repository")
	gitRef := ""
	skillPath := ""
	if len(location.treeParts) > 0 {
		gitRef = location.treeParts[0]
		skillPath = strings.Join(location.treeParts[1:], "/")
	}
	if err := cloneGitHubRepository(ctx, location, gitRef, checkout); err != nil {
		return nil, err
	}
	tracked, err := gitTrackedFiles(ctx, checkout)
	if err != nil {
		return nil, err
	}
	if skillPath == "" {
		candidates := make([]string, 0, 1)
		for _, trackedPath := range tracked {
			parts := strings.Split(trackedPath, "/")
			if trackedPath == skillManifestName || len(parts) == 4 && len(parts[0]) > 1 &&
				strings.HasPrefix(parts[0], ".") && parts[1] == "skills" &&
				parts[2] == ref.Name && parts[3] == skillManifestName {
				if len(candidates) >= remoteSkillMaxFiles {
					return nil, fmt.Errorf(
						"GitHub repository contains more than %d skill manifests",
						remoteSkillMaxFiles,
					)
				}
				candidates = append(candidates, trackedPath)
			}
		}
		skillPath, err = findGitHubSkillPath(ctx, checkout, candidates, ref.Name, false)
		if err != nil {
			return nil, err
		}
	}
	return filesFromGitHubCheckout(ctx, checkout, skillPath, tracked)
}

func cloneGitHubRepository(
	ctx context.Context,
	location githubSkillLocation,
	gitRef string,
	destination string,
) error {
	repository := "https://github.com/" + location.owner + "/" + location.repo + ".git"
	args := []string{"clone", "--depth", "1", "--single-branch"}
	if gitRef != "" {
		args = append(args, "--branch", gitRef)
	}
	args = append(args, repository, destination)
	command := exec.CommandContext(ctx, "git", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf(
			"git clone %s: %w: %s",
			repository,
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return nil
}

func parseGitHubSkillLocation(value string) (githubSkillLocation, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" ||
		!strings.EqualFold(parsed.Hostname(), "github.com") ||
		parsed.User != nil {
		return githubSkillLocation{}, fmt.Errorf("unsupported SkillsMP GitHub URL %q", value)
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	for index, part := range parts {
		decoded, decodeErr := url.PathUnescape(part)
		if decodeErr != nil || decoded == "" || decoded == "." || decoded == ".." ||
			strings.Contains(decoded, "/") || strings.Contains(decoded, "\\") {
			return githubSkillLocation{}, fmt.Errorf("invalid SkillsMP GitHub URL %q", value)
		}
		parts[index] = decoded
	}
	if len(parts) < 2 {
		return githubSkillLocation{}, fmt.Errorf("invalid SkillsMP GitHub URL %q", value)
	}
	location := githubSkillLocation{
		owner: parts[0],
		repo:  strings.TrimSuffix(parts[1], ".git"),
	}
	if location.repo == "" {
		return githubSkillLocation{}, fmt.Errorf("invalid SkillsMP GitHub URL %q", value)
	}
	if len(parts) == 2 {
		return location, nil
	}
	if len(parts) < 4 || parts[2] != "tree" {
		return githubSkillLocation{}, fmt.Errorf("unsupported SkillsMP GitHub URL %q", value)
	}
	location.treeParts = parts[3:]
	return location, nil
}

func filesFromGitHubCheckout(
	ctx context.Context,
	checkout string,
	skillPath string,
	tracked []string,
) ([]remoteSkillFile, error) {
	if skillPath != "" &&
		(skillPath != filepath.ToSlash(filepath.Clean(filepath.FromSlash(skillPath))) ||
			!filepath.IsLocal(filepath.FromSlash(skillPath))) {
		return nil, fmt.Errorf("GitHub skill path %q is unsafe", skillPath)
	}
	root := filepath.Join(checkout, filepath.FromSlash(skillPath))
	manifestOnly := skillPath == ""
	agentSkill := isGitHubAgentSkillPath(skillPath)
	var files []remoteSkillFile
	total := 0
	for _, trackedPath := range tracked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		relative := trackedPath
		if skillPath != "" {
			var ok bool
			relative, ok = strings.CutPrefix(trackedPath, skillPath+"/")
			if !ok {
				continue
			}
		}
		if relative == "" || manifestOnly && relative != skillManifestName ||
			agentSkill && !isSafeAgentSkillFile(relative) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if len(files) >= remoteSkillMaxFiles {
			return nil, fmt.Errorf(
				"GitHub skill contains more than %d files",
				remoteSkillMaxFiles,
			)
		}
		remaining := remoteSkillMaxBytes - total
		var contents []byte
		switch {
		case info.Mode().IsRegular():
			if info.Size() > int64(remaining) {
				return nil, fmt.Errorf(
					"GitHub skill exceeds %d bytes",
					remoteSkillMaxBytes,
				)
			}
			contents, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read GitHub skill file %q: %w", relative, err)
			}
		case relative == "CLAUDE.md" && info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return nil, fmt.Errorf("read GitHub skill link %q: %w", relative, readErr)
			}
			if target != "AGENTS.md" {
				return nil, fmt.Errorf(
					"GitHub skill link %q targets %q; want %q",
					relative,
					target,
					"AGENTS.md",
				)
			}
			contents = []byte(target)
		default:
			return nil, fmt.Errorf("GitHub skill contains non-regular path %q", relative)
		}
		if len(contents) > remaining {
			return nil, fmt.Errorf(
				"GitHub skill exceeds %d bytes",
				remoteSkillMaxBytes,
			)
		}
		total += len(contents)
		files = append(files, remoteSkillFile{
			Path:     relative,
			Contents: contents,
			Mode:     info.Mode(),
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("GitHub repository does not contain skill path %q", skillPath)
	}
	return files, nil
}

func isGitHubAgentSkillPath(skillPath string) bool {
	parts := strings.Split(skillPath, "/")
	return len(parts) == 3 && len(parts[0]) > 1 && strings.HasPrefix(parts[0], ".") &&
		parts[1] == "skills"
}

func isSafeAgentSkillFile(relative string) bool {
	if relative == skillManifestName {
		return true
	}
	directory, _, nested := strings.Cut(relative, "/")
	if !nested {
		return false
	}
	return directory == "references" || directory == "scripts" || directory == "assets" || directory == "data"
}

// gitHubSkillSearchFiles limits discovery to an explicit source path, or to
// SKILL.md files whose directory is the catalog skill name.
func gitHubSkillSearchFiles(tracked []string, sourcePath, name string) []string {
	if sourcePath != "" {
		prefix := sourcePath + "/"
		files := make([]string, 0)
		for _, trackedPath := range tracked {
			if trackedPath == sourcePath || strings.HasPrefix(trackedPath, prefix) {
				files = append(files, trackedPath)
			}
		}
		return files
	}
	named := make([]string, 0)
	for _, trackedPath := range tracked {
		if filepath.Base(filepath.FromSlash(trackedPath)) != skillManifestName {
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(trackedPath)))
		if filepath.Base(filepath.FromSlash(directory)) == name {
			named = append(named, trackedPath)
		}
	}
	if len(named) > 0 {
		return named
	}
	return tracked
}

func findGitHubSkillPath(
	ctx context.Context,
	checkout string,
	tracked []string,
	name string,
	firstMatch bool,
) (string, error) {
	manifests := make([]string, 0)
	for _, trackedPath := range tracked {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if filepath.Base(filepath.FromSlash(trackedPath)) == skillManifestName {
			manifests = append(manifests, trackedPath)
			if len(manifests) > remoteSkillMaxFiles {
				return "", fmt.Errorf(
					"GitHub repository contains more than %d skill manifests",
					remoteSkillMaxFiles,
				)
			}
		}
	}

	found := ""
	matched := false
	total := 0
	for _, trackedPath := range manifests {
		relative, err := validRemoteFilePath(trackedPath)
		if err != nil {
			return "", fmt.Errorf("GitHub repository: %w", err)
		}
		path := filepath.Join(checkout, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("inspect GitHub skill manifest %q: %w", relative, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("GitHub skill manifest %q is not a regular file", relative)
		}
		remaining := remoteSkillMaxBytes - total
		if info.Size() > int64(remaining) {
			return "", fmt.Errorf(
				"GitHub skill manifests exceed %d bytes",
				remoteSkillMaxBytes,
			)
		}
		total += int(info.Size())
		skill, ok, err := parseSkill(path)
		if err != nil {
			return "", fmt.Errorf("parse GitHub skill manifest %q: %w", relative, err)
		}
		if !ok || skill.Name != name {
			continue
		}
		candidate := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
		if candidate == "." {
			candidate = ""
		}
		if firstMatch {
			return candidate, nil
		}
		if matched {
			return "", fmt.Errorf(
				"GitHub repository contains multiple skills named %q",
				name,
			)
		}
		found = candidate
		matched = true
	}
	if !matched {
		return "", fmt.Errorf("GitHub repository does not contain skill %q", name)
	}
	return found, nil
}

func gitTrackedFiles(ctx context.Context, checkout string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, err := exec.CommandContext(
		ctx,
		"git",
		"-C",
		checkout,
		"ls-files",
		"-z",
	).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("list cloned GitHub files: %w", err)
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		return nil, nil
	}
	return paths, nil
}

func (r *skillsMPRegistry) searchSkills(
	ctx context.Context,
	query string,
) ([]skillsMPSkill, error) {
	r.mu.Lock()
	query = normalizedRegistryQuery(query)
	cache, err := loadSkillsMPCache(r.searchCachePath)
	if err == nil {
		removeExpiredSkillsMPSearches(cache.Searches, time.Now())
	}
	cached, exists := cache.Searches[query]
	r.mu.Unlock()
	if err == nil && exists {
		return uniqueSkillsMP(cached.Skills), nil
	}
	skills, err := r.fetch(ctx, "/api/v1/skills/search", url.Values{
		"q":      {query},
		"page":   {"1"},
		"limit":  {"100"},
		"sortBy": {"stars"},
	}, true)
	if err != nil {
		return nil, err
	}
	if r.searchCachePath == "" {
		return skills, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cache, err = loadSkillsMPCache(r.searchCachePath)
	if err != nil {
		cache = skillsMPCache{}
	}
	if cache.SchemaRevision == 0 {
		cache.SchemaRevision = skillsMPSchemaRevision
	}
	if cache.Searches == nil {
		cache.Searches = make(map[string]skillsMPSearchResult)
	}
	cache.Searches[query] = skillsMPSearchResult{
		UpdatedAt: time.Now().UTC(),
		Skills:    skills,
	}
	removeExpiredSkillsMPSearches(cache.Searches, time.Now())
	trimSkillsMPSearches(cache.Searches)
	return skills, saveSkillsMPCache(r.searchCachePath, cache)
}

func trimSkillsMPSearches(searches map[string]skillsMPSearchResult) {
	for len(searches) > skillsMPMaxSearches {
		var oldestQuery string
		var oldest time.Time
		for query, result := range searches {
			if oldestQuery == "" || result.UpdatedAt.Before(oldest) {
				oldestQuery = query
				oldest = result.UpdatedAt
			}
		}
		delete(searches, oldestQuery)
	}
}

func removeExpiredSkillsMPSearches(
	searches map[string]skillsMPSearchResult,
	now time.Time,
) {
	for query, result := range searches {
		if !now.Before(result.UpdatedAt.Add(skillsMPCacheTTL)) {
			delete(searches, query)
		}
	}
}

func (r *skillsMPRegistry) fetch(
	ctx context.Context,
	path string,
	query url.Values,
	nested bool,
) ([]skillsMPSkill, error) {
	endpoint := r.baseURL + path + "?" + query.Encode()
	authorization := ""
	if r.apiKey != "" {
		authorization = "Bearer " + r.apiKey
	}
	var result struct {
		Skills []skillsMPSkill `json:"skills"`
		Data   struct {
			Skills []skillsMPSkill `json:"skills"`
		} `json:"data"`
	}
	if err := fetchRegistryJSON(ctx, r.client, endpoint, authorization, &result); err != nil {
		return nil, err
	}
	skills := result.Skills
	if nested {
		skills = result.Data.Skills
	}
	return uniqueSkillsMP(skills), nil
}

func uniqueSkillsMP(skills []skillsMPSkill) []skillsMPSkill {
	type identity struct {
		author string
		name   string
	}
	seenIDs := make(map[string]struct{}, len(skills))
	seenIdentities := make(map[identity]struct{}, len(skills))
	valid := skills[:0]
	for _, skill := range skills {
		if skill.ID == "" || skill.Name == "" {
			continue
		}
		key := identity{author: skill.Author, name: skill.Name}
		_, seenID := seenIDs[skill.ID]
		_, seenIdentity := seenIdentities[key]
		seenIDs[skill.ID] = struct{}{}
		seenIdentities[key] = struct{}{}
		if seenID || seenIdentity {
			continue
		}
		valid = append(valid, skill)
	}
	return valid
}

func loadSkillsMPCache(path string) (skillsMPCache, error) {
	if path == "" {
		return skillsMPCache{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skillsMPCache{}, nil
		}
		return skillsMPCache{}, fmt.Errorf("open SkillsMP cache: %w", err)
	}
	defer file.Close()
	data, err := readBoundedFile(file, remoteResponseLimit)
	if err != nil {
		return skillsMPCache{}, fmt.Errorf("read SkillsMP cache: %w", err)
	}
	var cache skillsMPCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return skillsMPCache{}, fmt.Errorf("decode SkillsMP cache: %w", err)
	}
	if cache.SchemaRevision != skillsMPSchemaRevision {
		return skillsMPCache{}, fmt.Errorf(
			"decode SkillsMP cache: unsupported schema revision %d",
			cache.SchemaRevision,
		)
	}
	return cache, nil
}

func saveSkillsMPCache(path string, cache skillsMPCache) error {
	return writeAtomicJSONFile(path, "SkillsMP cache", cache)
}
