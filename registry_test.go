package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteRegistryRefreshesTopicCache(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/search" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("q") == "" || request.URL.Query().Get("limit") != "200" {
			t.Errorf("request query = %q", request.URL.RawQuery)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("unexpected authorization header = %q", authorization)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"skills": []map[string]any{
				{
					"id": "owner/repo/zeta", "slug": "zeta", "name": "zeta",
					"source": "owner/repo", "installs": 2,
				},
				{
					"id": "owner/repo/alpha", "slug": "alpha", "name": "Alpha",
					"source": "owner/repo", "installs": 4,
				},
				{
					"id": "owner/repo/alpha", "slug": "alpha", "name": "Alpha copy",
					"source": "owner/repo", "installs": 4,
				},
			},
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "cache", "skills-sh.json")
	registry := &remoteRegistry{
		baseURL: server.URL, cachePath: path, client: server.Client(),
	}
	if err := registry.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests != len(skillsRegistryTopics) {
		t.Fatalf("requests = %d, want %d", requests, len(skillsRegistryTopics))
	}
	cache, err := loadRemoteCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if cache.SchemaRevision != remoteRegistrySchemaRevision ||
		len(cache.Topics) != len(skillsRegistryTopics) ||
		cache.UpdatedAt.IsZero() {
		t.Fatalf("cache metadata = %#v", cache)
	}
	for _, topic := range cache.Topics {
		if len(topic.Skills) != 2 ||
			topic.Skills[0].Name != "Alpha" ||
			topic.Skills[1].Name != "zeta" {
			t.Fatalf("topic %q skills = %#v", topic.Name, topic.Skills)
		}
	}
}

func TestRemoteRegistryPreservesCacheWhenRefreshFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills-sh.json")
	original := remoteRegistryCache{
		SchemaRevision: remoteRegistrySchemaRevision,
		Topics:         []remoteTopic{{Slug: "testing", Name: "Testing"}},
	}
	if err := saveRemoteCache(path, original); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, `{"error":"unavailable","message":"try later"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	registry := &remoteRegistry{
		baseURL: server.URL, cachePath: path, client: server.Client(),
	}
	err := registry.refresh(t.Context())
	if err == nil || !strings.Contains(err.Error(), "try later") {
		t.Fatalf("refresh error = %v", err)
	}
	cache, loadErr := loadRemoteCache(path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(cache.Topics) != 1 || cache.Topics[0].Name != "Testing" {
		t.Fatalf("failed refresh replaced cache: %#v", cache)
	}
}

func TestDaemonRefreshFailureIsReportedWithoutReturning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, `{"error":"unavailable","message":"try later"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	registry := &remoteRegistry{
		baseURL: server.URL, cachePath: filepath.Join(t.TempDir(), "skills-sh.json"),
		client: server.Client(),
	}
	var stderr strings.Builder
	refreshRemoteRegistry(t.Context(), registry, &stderr)
	if !strings.Contains(stderr.String(), "try later") {
		t.Fatalf("daemon diagnostic = %q", stderr.String())
	}
}
