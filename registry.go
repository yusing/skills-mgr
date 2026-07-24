package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	remoteRegistrySchemaRevision = 1
	remoteResponseLimit          = 4 << 20
	skillsRegistryURL            = "https://skills.sh"
)

var skillsRegistryTopics = []remoteTopicSpec{
	{Slug: "react", Name: "Frontend & React", Query: "react"},
	{Slug: "nextjs", Name: "Next.js", Query: "next.js"},
	{Slug: "design", Name: "Design & UI", Query: "design ui"},
	{Slug: "mobile", Name: "Mobile", Query: "mobile"},
	{Slug: "agent-workflows", Name: "Agent workflows", Query: "agent workflows"},
	{Slug: "databases", Name: "Databases", Query: "databases"},
	{Slug: "testing", Name: "Testing", Query: "testing"},
	{Slug: "marketing", Name: "Marketing", Query: "marketing"},
}

type remoteTopicSpec struct {
	Slug  string
	Name  string
	Query string
}

type remoteRegistryCache struct {
	SchemaRevision int           `json:"schemaRevision"`
	UpdatedAt      time.Time     `json:"updatedAt"`
	Topics         []remoteTopic `json:"topics"`
}

type remoteTopic struct {
	Slug   string        `json:"slug"`
	Name   string        `json:"name"`
	Skills []remoteSkill `json:"skills"`
}

type remoteSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Installs    int    `json:"installs"`
}

func (s remoteSkill) ref() remoteSkillRef {
	return remoteSkillRef{
		Provider: skillsShProvider,
		ID:       s.ID,
		Name:     s.Name,
		Locator:  s.ID,
	}
}

type remoteRegistry struct {
	baseURL   string
	cachePath string
	client    *http.Client
}

func newRemoteRegistry(cachePath string) *remoteRegistry {
	return &remoteRegistry{
		baseURL:   skillsRegistryURL,
		cachePath: cachePath,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *remoteRegistry) refresh(ctx context.Context) error {
	cache := remoteRegistryCache{
		SchemaRevision: remoteRegistrySchemaRevision,
		UpdatedAt:      time.Now().UTC(),
		Topics:         make([]remoteTopic, 0, len(skillsRegistryTopics)),
	}
	for _, spec := range skillsRegistryTopics {
		skills, err := r.fetch(ctx, spec.Query)
		if err != nil {
			return fmt.Errorf("refresh %s topic: %w", spec.Name, err)
		}
		cache.Topics = append(cache.Topics, remoteTopic{
			Slug: spec.Slug, Name: spec.Name, Skills: skills,
		})
	}
	return saveRemoteCache(r.cachePath, cache)
}

func (r *remoteRegistry) search(
	ctx context.Context,
	query string,
) ([]registrySearchSkill, error) {
	skills, err := r.fetch(ctx, normalizedRegistryQuery(query))
	if err != nil {
		return nil, err
	}
	results := make([]registrySearchSkill, len(skills))
	for index, skill := range skills {
		results[index] = registrySearchSkill{
			ID:          skill.ID,
			Name:        skill.Name,
			Description: skill.Description,
			Label:       fmt.Sprintf("%s • %d installs", skill.Source, skill.Installs),
			Provider:    skillsShProvider,
			Locator:     skill.ID,
		}
	}
	return results, nil
}

func (r *remoteRegistry) fetchSkill(
	ctx context.Context,
	ref remoteSkillRef,
) ([]remoteSkillFile, error) {
	if ref.Provider != skillsShProvider || ref.Locator != ref.ID {
		return nil, fmt.Errorf("invalid skills.sh skill reference")
	}
	parts := strings.Split(ref.ID, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid skills.sh skill identifier %q", ref.ID)
	}
	for _, part := range parts[:2] {
		if !validGitHubRepositoryPart(part) {
			return nil, fmt.Errorf("invalid skills.sh skill identifier %q", ref.ID)
		}
	}

	temporary, err := os.MkdirTemp("", "skills-mgr-clone-")
	if err != nil {
		return nil, fmt.Errorf("create clone directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	checkout := filepath.Join(temporary, "repository")
	location := githubSkillLocation{owner: parts[0], repo: parts[1]}
	if err := cloneGitHubRepository(ctx, location, "", checkout); err != nil {
		return nil, err
	}
	tracked, err := gitTrackedFiles(ctx, checkout)
	if err != nil {
		return nil, err
	}
	skillPath, err := findGitHubSkillPath(ctx, checkout, tracked, ref.Name)
	if err != nil {
		return nil, err
	}
	return filesFromGitHubCheckout(ctx, checkout, skillPath, tracked)
}

func validGitHubRepositoryPart(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func (r *remoteRegistry) fetch(ctx context.Context, query string) ([]remoteSkill, error) {
	endpoint := r.baseURL + "/api/search?q=" + url.QueryEscape(query) + "&limit=200"
	var result struct {
		Skills []remoteSkill `json:"skills"`
	}
	if err := fetchRegistryJSON(ctx, r.client, endpoint, "", &result); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(result.Skills))
	skills := result.Skills[:0]
	for _, skill := range result.Skills {
		if skill.ID == "" || skill.Name == "" {
			continue
		}
		if _, exists := seen[skill.ID]; exists {
			continue
		}
		seen[skill.ID] = struct{}{}
		skills = append(skills, skill)
	}
	sortRemoteSkills(skills)
	return skills, nil
}

func sortRemoteSkills(skills []remoteSkill) {
	slices.SortFunc(skills, func(a, b remoteSkill) int {
		if installs := cmp.Compare(b.Installs, a.Installs); installs != 0 {
			return installs
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
}

func loadRemoteCache(path string) (remoteRegistryCache, error) {
	if path == "" {
		return remoteRegistryCache{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return remoteRegistryCache{}, nil
		}
		return remoteRegistryCache{}, fmt.Errorf("open remote registry cache: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, remoteResponseLimit+1))
	if err != nil {
		return remoteRegistryCache{}, fmt.Errorf("read remote registry cache: %w", err)
	}
	if len(data) > remoteResponseLimit {
		return remoteRegistryCache{}, fmt.Errorf(
			"decode remote registry cache: file exceeds %d bytes",
			remoteResponseLimit,
		)
	}
	var cache remoteRegistryCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return remoteRegistryCache{}, fmt.Errorf("decode remote registry cache: %w", err)
	}
	if cache.SchemaRevision != remoteRegistrySchemaRevision {
		return remoteRegistryCache{}, fmt.Errorf(
			"decode remote registry cache: unsupported schema revision %d",
			cache.SchemaRevision,
		)
	}
	for index := range cache.Topics {
		sortRemoteSkills(cache.Topics[index].Skills)
	}
	return cache, nil
}

func saveRemoteCache(path string, cache remoteRegistryCache) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create remote registry cache directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".skills-sh-")
	if err != nil {
		return fmt.Errorf("create remote registry cache: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(cache)
	if err == nil {
		err = temp.Chmod(0o600)
	}
	err = errors.Join(err, temp.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("write remote registry cache: %w", err)
	}
	return nil
}
