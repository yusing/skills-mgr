package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
					"source": "owner/repo", "installs": 8,
				},
				{
					"id": "owner/repo/alpha", "slug": "alpha", "name": "Alpha",
					"source": "owner/repo", "installs": 4,
				},
				{
					"id": "owner/repo/beta", "slug": "beta", "name": "beta",
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
		if len(topic.Skills) != 3 ||
			topic.Skills[0].Name != "zeta" ||
			topic.Skills[1].Name != "Alpha" ||
			topic.Skills[2].Name != "beta" {
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

func TestLoadRemoteCacheSortsSkillsByInstalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills-sh.json")
	cache := remoteRegistryCache{
		SchemaRevision: remoteRegistrySchemaRevision,
		Topics: []remoteTopic{{
			Slug: "testing",
			Name: "Testing",
			Skills: []remoteSkill{
				{ID: "owner/repo/alpha", Name: "alpha", Installs: 2},
				{ID: "owner/repo/zeta", Name: "zeta", Installs: 8},
			},
		}},
	}
	if err := saveRemoteCache(path, cache); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadRemoteCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Topics[0].Skills[0].Name; got != "zeta" {
		t.Fatalf("first cached skill = %q, want zeta", got)
	}
}

func TestRemoteRegistrySearchNormalizesAndValidatesResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("q") != "go testing" {
			t.Errorf("query = %q", request.URL.Query().Get("q"))
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"skills": []map[string]any{
				{
					"id": "owner/repo/testing", "name": "Testing",
					"source": "owner/repo", "installs": 9,
				},
				{"id": "owner/repo/testing", "name": "collision"},
				{"id": "", "name": "missing identifier"},
				{"id": "owner/repo/missing-name", "name": ""},
			},
		})
	}))
	defer server.Close()
	registry := &remoteRegistry{baseURL: server.URL, client: server.Client()}

	skills, err := registry.search(t.Context(), "  GO   TESTING ")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].ID != "owner/repo/testing" ||
		skills[0].Label != "owner/repo • 9 installs" {
		t.Fatalf("skills = %#v", skills)
	}
}

func TestSkillsShFetchSkillClonesPublicRepositoryAndIgnoresUnrelatedSkill(t *testing.T) {
	t.Setenv("VERCEL_OIDC_TOKEN", "")
	logPath := fakeGit(t, map[string]map[string]gitTestFile{
		"default": {
			"packages/alpha/SKILL.md": {
				contents: skillFile("alpha", "Alpha.", "body"),
				mode:     0o644,
			},
			"packages/alpha/scripts/check.sh": {
				contents: "#!/bin/sh\n",
				mode:     0o755,
			},
			"packages/beta/SKILL.md": {
				contents: skillFile("beta", "Beta.", "other"),
				mode:     0o644,
			},
		},
	})
	files, err := newRemoteRegistry("").fetchSkill(t.Context(), remoteSkillRef{
		Provider: skillsShProvider,
		ID:       "owner/repo/registry-slug",
		Name:     "alpha",
		Locator:  "owner/repo/registry-slug",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "SKILL.md" ||
		files[1].Path != "scripts/check.sh" ||
		files[1].Mode&0o111 == 0 {
		t.Fatalf("fetched files = %#v", files)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(
		string(logged),
		"clone --depth 1 --single-branch https://github.com/owner/repo.git ",
	) || strings.Count(string(logged), "\n") != 1 {
		t.Fatalf("git invocation = %q", logged)
	}
}

func TestSkillsShFetchSkillRejectsMalformedAndUnknownReferences(t *testing.T) {
	tests := []struct {
		name string
		ref  remoteSkillRef
	}{
		{
			name: "missing skill segment",
			ref: remoteSkillRef{
				Provider: skillsShProvider,
				ID:       "owner/repo",
				Name:     "alpha",
				Locator:  "owner/repo",
			},
		},
		{
			name: "mismatched locator",
			ref: remoteSkillRef{
				Provider: skillsShProvider,
				ID:       "owner/repo/alpha",
				Name:     "alpha",
				Locator:  "owner/repo/beta",
			},
		},
		{
			name: "unsafe repository",
			ref: remoteSkillRef{
				Provider: skillsShProvider,
				ID:       "owner/repo?ref=main/alpha",
				Name:     "alpha",
				Locator:  "owner/repo?ref=main/alpha",
			},
		},
		{
			name: "future provider",
			ref: remoteSkillRef{
				Provider: "skills.sh.v2",
				ID:       "owner/repo/alpha",
				Name:     "alpha",
				Locator:  "owner/repo/alpha",
			},
		},
	}
	registry := newRemoteRegistry("")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := registry.fetchSkill(t.Context(), test.ref); err == nil {
				t.Fatal("fetchSkill accepted invalid reference")
			}
		})
	}
}

func TestGitHubSkillPostCloneOperationsHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checks := []struct {
		name string
		run  func() error
	}{
		{
			name: "tracked files",
			run: func() error {
				_, err := gitTrackedFiles(ctx, t.TempDir())
				return err
			},
		},
		{
			name: "named skill discovery",
			run: func() error {
				_, err := findGitHubSkillPath(
					ctx,
					t.TempDir(),
					[]string{"SKILL.md"},
					"alpha",
				)
				return err
			},
		},
		{
			name: "subtree extraction",
			run: func() error {
				_, err := filesFromGitHubCheckout(
					ctx,
					t.TempDir(),
					"",
					[]string{"SKILL.md"},
				)
				return err
			},
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
		})
	}
}

func TestGitHubSkillDiscoveryRejectsExcessiveManifests(t *testing.T) {
	tracked := make([]string, remoteSkillMaxFiles+1)
	for index := range tracked {
		tracked[index] = fmt.Sprintf("skills/%d/SKILL.md", index)
	}
	_, err := findGitHubSkillPath(t.Context(), t.TempDir(), tracked, "alpha")
	if err == nil || !strings.Contains(err.Error(), "more than 1024 skill manifests") {
		t.Fatalf("excessive manifest error = %v", err)
	}
}

func TestSkillsShFetchSkillRejectsMissingAndDuplicateDeclaredNames(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]gitTestFile
		want  string
	}{
		{
			name: "missing",
			files: map[string]gitTestFile{
				"skills/beta/SKILL.md": {
					contents: skillFile("beta", "Beta.", "body"),
					mode:     0o644,
				},
			},
			want: `does not contain skill "alpha"`,
		},
		{
			name: "duplicate",
			files: map[string]gitTestFile{
				"one/SKILL.md": {
					contents: skillFile("alpha", "Alpha one.", "body"),
					mode:     0o644,
				},
				"two/SKILL.md": {
					contents: skillFile("alpha", "Alpha two.", "body"),
					mode:     0o644,
				},
			},
			want: `multiple skills named "alpha"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeGit(t, map[string]map[string]gitTestFile{"default": test.files})
			_, err := newRemoteRegistry("").fetchSkill(t.Context(), remoteSkillRef{
				Provider: skillsShProvider,
				ID:       "owner/repo/alpha",
				Name:     "alpha",
				Locator:  "owner/repo/alpha",
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("fetchSkill error = %v, want %q", err, test.want)
			}
		})
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
	logger, logs := testLogger()
	err := refreshRemoteRegistry(t.Context(), registry, logger, "command")
	if err == nil || !strings.Contains(err.Error(), "try later") {
		t.Fatalf("refresh error = %v", err)
	}
	if !strings.Contains(logs.String(), "try later") ||
		!strings.Contains(logs.String(), "registry cache refresh failed") {
		t.Fatalf("daemon diagnostic = %q", logs.String())
	}
}
