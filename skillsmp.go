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
	if err != nil {
		return skills, err
	}
	return skills, nil
}

func (r *skillsMPRegistry) search(ctx context.Context, query string) ([]skillsMPSkill, error) {
	r.mu.Lock()
	query = normalizedSkillsMPQuery(query)
	cache, err := loadSkillsMPCache(r.searchCachePath)
	if err == nil {
		removeExpiredSkillsMPSearches(cache.Searches, time.Now())
	}
	cached, exists := cache.Searches[query]
	r.mu.Unlock()
	if err == nil && exists {
		return cached.Skills, nil
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
	if err := saveSkillsMPCache(r.searchCachePath, cache); err != nil {
		return skills, err
	}
	return skills, nil
}

func normalizedSkillsMPQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, remoteResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > remoteResponseLimit {
		return nil, fmt.Errorf("response exceeds %d bytes", remoteResponseLimit)
	}
	if response.StatusCode != http.StatusOK {
		var apiError struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apiError) == nil {
			message := apiError.Error.Message
			if message == "" {
				message = apiError.Message
			}
			if message != "" {
				return nil, fmt.Errorf("%s: %s", response.Status, message)
			}
		}
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}

	var result struct {
		Skills []skillsMPSkill `json:"skills"`
		Data   struct {
			Skills []skillsMPSkill `json:"skills"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	skills := result.Skills
	if nested {
		skills = result.Data.Skills
	}
	seen := make(map[string]struct{}, len(skills))
	valid := skills[:0]
	for _, skill := range skills {
		if skill.ID == "" || skill.Name == "" {
			continue
		}
		if _, exists := seen[skill.ID]; exists {
			continue
		}
		seen[skill.ID] = struct{}{}
		valid = append(valid, skill)
	}
	return valid, nil
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
	data, err := io.ReadAll(io.LimitReader(file, remoteResponseLimit+1))
	if err != nil {
		return skillsMPCache{}, fmt.Errorf("read SkillsMP cache: %w", err)
	}
	if len(data) > remoteResponseLimit {
		return skillsMPCache{}, fmt.Errorf(
			"decode SkillsMP cache: file exceeds %d bytes",
			remoteResponseLimit,
		)
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
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create SkillsMP cache directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".skillsmp-")
	if err != nil {
		return fmt.Errorf("create SkillsMP cache: %w", err)
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
		return fmt.Errorf("write SkillsMP cache: %w", err)
	}
	return nil
}
