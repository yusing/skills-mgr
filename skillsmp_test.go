package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSkillsMPRefreshesDefaultCatalogWithoutAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/skills" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("page") != "1" || query.Get("limit") != "48" ||
			query.Get("sortBy") != "stars" {
			t.Errorf("request query = %q", request.URL.RawQuery)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("unexpected authorization header = %q", authorization)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"skills": []map[string]any{
				{
					"id": "alpha-id", "name": "alpha", "author": "owner",
					"description": "Alpha skill", "githubUrl": "https://example.test/alpha",
					"stars": 42,
				},
				{"id": "alpha-id", "name": "duplicate"},
				{
					"id": "localized-alpha-id", "name": "alpha", "author": "owner",
					"githubUrl": "https://example.test/localized-alpha",
				},
				{"id": "", "name": "missing-id"},
			},
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "cache", "skillsmp.json")
	registry := newSkillsMPRegistry(path, "")
	registry.baseURL = server.URL
	registry.client = server.Client()
	skills, err := registry.catalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("catalog = %#v", skills)
	}
	if skills[0].GitHubURL != "https://example.test/alpha" {
		t.Fatalf("catalog kept lower-ranked duplicate = %#v", skills)
	}
	cache, err := loadSkillsMPCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if cache.SchemaRevision != skillsMPSchemaRevision || cache.UpdatedAt.IsZero() ||
		len(cache.Skills) != 1 || cache.Skills[0].Stars != 42 {
		t.Fatalf("cache = %#v", cache)
	}
}

func TestUniqueSkillsMPReservesDuplicateIDsAndIdentities(t *testing.T) {
	tests := []struct {
		name   string
		skills []skillsMPSkill
	}{
		{
			name: "identity duplicate followed by ID duplicate",
			skills: []skillsMPSkill{
				{ID: "first", Name: "alpha", Author: "owner"},
				{ID: "second", Name: "alpha", Author: "owner"},
				{ID: "second", Name: "beta", Author: "other"},
			},
		},
		{
			name: "ID duplicate followed by identity duplicate",
			skills: []skillsMPSkill{
				{ID: "first", Name: "alpha", Author: "owner"},
				{ID: "first", Name: "beta", Author: "other"},
				{ID: "second", Name: "beta", Author: "other"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := uniqueSkillsMP(test.skills)
			if len(got) != 1 || got[0].ID != "first" {
				t.Fatalf("uniqueSkillsMP() = %#v", got)
			}
		})
	}
}

func TestSkillsMPSearchUsesAPIAndProvidedKey(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/v1/skills/search" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("q") != "go testing" || query.Get("page") != "1" ||
			query.Get("limit") != "50" || query.Get("sortBy") != "stars" {
			t.Errorf("request query = %q", request.URL.RawQuery)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer secret" {
			t.Errorf("authorization header = %q", authorization)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"skills": []map[string]any{{
					"id": "testing-id", "name": "testing", "author": "gopher",
					"description": "Tests Go packages.", "stars": 99,
				}},
			},
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "skillsmp.json")
	registry := newSkillsMPRegistry(path, "secret")
	registry.baseURL = server.URL
	registry.client = server.Client()
	skills, err := registry.search(t.Context(), "go testing")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "testing" ||
		skills[0].Description != "Tests Go packages." ||
		skills[0].Label != "gopher • 99 stars" {
		t.Fatalf("skills = %#v", skills)
	}
	cached, err := registry.search(t.Context(), "  GO   TESTING ")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(cached) != 1 || cached[0].ID != "testing-id" {
		t.Fatalf("cached search made %d requests and returned %#v", requests, cached)
	}
	restarted := newSkillsMPRegistry(path, "secret")
	restarted.baseURL = server.URL
	restarted.client = server.Client()
	if _, err := restarted.search(t.Context(), "go testing"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("persisted cache made %d requests, want 1", requests)
	}
	cache, err := loadSkillsMPCache(registry.searchCachePath)
	if err != nil {
		t.Fatal(err)
	}
	result := cache.Searches["go testing"]
	result.UpdatedAt = time.Now().Add(-skillsMPCacheTTL - time.Second)
	cache.Searches["go testing"] = result
	if err := saveSkillsMPCache(registry.searchCachePath, cache); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.search(t.Context(), "go testing"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expired cache made %d requests, want 2", requests)
	}
}

func TestSkillsMPSearchPreservesResultsWhenCacheWriteFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{
			"data": map[string]any{
				"skills": []map[string]any{{
					"id": "result-id", "name": "result", "author": "owner", "stars": 3,
				}},
			},
		})
	}))
	defer server.Close()

	blockedDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDirectory, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := newSkillsMPRegistry(
		filepath.Join(blockedDirectory, "skillsmp.json"),
		"",
	)
	registry.baseURL = server.URL
	registry.client = server.Client()

	skills, err := registry.search(t.Context(), "result")
	if err == nil || !strings.Contains(err.Error(), "create SkillsMP cache directory") {
		t.Fatalf("error = %v", err)
	}
	if len(skills) != 1 || skills[0].ID != "result-id" {
		t.Fatalf("cache error discarded fetched skills: %#v", skills)
	}
}

func TestLoadSkillsMPCacheHandlesMissingAndInvalidFiles(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	cache, err := loadSkillsMPCache(missing)
	if err != nil {
		t.Fatal(err)
	}
	if cache.SchemaRevision != 0 || len(cache.Skills) != 0 || len(cache.Searches) != 0 {
		t.Fatalf("missing SkillsMP cache = %#v", cache)
	}

	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{name: "oversized", contents: strings.Repeat(" ", remoteResponseLimit+1), want: "exceeds"},
		{name: "malformed", contents: "{", want: "decode SkillsMP cache"},
		{
			name:     "unsupported schema",
			contents: `{"schemaRevision":2,"skills":[]}`,
			want:     "unsupported schema revision 2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "skillsmp.json")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadSkillsMPCache(path); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("load SkillsMP cache error = %v", err)
			}
		})
	}
}
