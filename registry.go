package main

import (
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	Installs int    `json:"installs"`
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
			ID:    skill.ID,
			Name:  skill.Name,
			Label: fmt.Sprintf("%s • %d installs", skill.Source, skill.Installs),
		}
	}
	return results, nil
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
	slices.SortFunc(skills, func(a, b remoteSkill) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return skills, nil
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
