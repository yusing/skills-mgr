package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	skillsShProvider   = "skills.sh"
	skillsMPProvider   = "SkillsMP"
	skillsMPCacheTTL   = 5 * time.Minute
	registryCacheLimit = 4 << 20
)

type registrySearchSkill struct {
	ID          string
	Name        string
	Description string
	Label       string
	Provider    string
	Locator     string
}

func (s registrySearchSkill) ref() remoteSkillRef {
	return remoteSkillRef{
		Provider: s.Provider,
		ID:       s.ID,
		Name:     s.Name,
		Locator:  s.Locator,
	}
}

func normalizedRegistryQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
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

func sortRemoteSkills(skills []remoteSkill) {
	slices.SortFunc(skills, func(a, b remoteSkill) int {
		if installs := cmp.Compare(b.Installs, a.Installs); installs != 0 {
			return installs
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
}

type skillsMPCache struct {
	SchemaRevision int             `json:"schemaRevision"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Skills         []skillsMPSkill `json:"skills"`
}

type skillsMPSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Author      string `json:"author"`
	Description string `json:"description"`
	GitHubURL   string `json:"githubUrl"`
	Stars       int    `json:"stars"`
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

// These are read-only views of the registry caches the core publishes atomically.
func readRegistryCache(path, label string, cache any, revision *int) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, registryCacheLimit+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", label, err)
	}
	if len(data) > registryCacheLimit {
		return fmt.Errorf("read %s: cache too large", label)
	}
	if err := json.Unmarshal(data, cache); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	if *revision != 1 {
		return fmt.Errorf("decode %s: unsupported schema revision %d", label, *revision)
	}
	return nil
}

func loadRemoteCache(path string) (remoteRegistryCache, error) {
	var cache remoteRegistryCache
	if err := readRegistryCache(path, "remote registry cache", &cache, &cache.SchemaRevision); err != nil {
		return remoteRegistryCache{}, err
	}
	for index := range cache.Topics {
		sortRemoteSkills(cache.Topics[index].Skills)
	}
	return cache, nil
}

func loadSkillsMPCache(path string) (skillsMPCache, error) {
	var cache skillsMPCache
	err := readRegistryCache(path, "SkillsMP cache", &cache, &cache.SchemaRevision)
	return cache, err
}
