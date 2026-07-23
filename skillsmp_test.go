package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSkillsMPRefreshesDefaultCatalogWithoutAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/skills" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("page") != "1" || query.Get("limit") != "200" ||
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
	cache, err := loadSkillsMPCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if cache.SchemaRevision != skillsMPSchemaRevision || cache.UpdatedAt.IsZero() ||
		len(cache.Skills) != 1 || cache.Skills[0].Stars != 42 {
		t.Fatalf("cache = %#v", cache)
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
			query.Get("limit") != "100" || query.Get("sortBy") != "stars" {
			t.Errorf("request query = %q", request.URL.RawQuery)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer secret" {
			t.Errorf("authorization header = %q", authorization)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"skills": []map[string]any{{
					"id": "testing-id", "name": "testing", "author": "gopher", "stars": 99,
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
	if len(skills) != 1 || skills[0].Name != "testing" || skills[0].Author != "gopher" {
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
