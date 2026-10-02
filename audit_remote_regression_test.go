package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type auditCountingProvider struct {
	staticRemoteProvider
	calls int
}

func (p *auditCountingProvider) fetchSkill(ctx context.Context, ref remoteSkillRef) ([]remoteSkillFile, error) {
	p.calls++
	return p.staticRemoteProvider.fetchSkill(ctx, ref)
}

func auditRemoteRef(name string) remoteSkillRef {
	return remoteSkillRef{Provider: skillsShProvider, ID: "owner/repo/" + name, Name: name, Locator: "owner/repo/" + name}
}

func auditProvider(name string) *auditCountingProvider {
	return &auditCountingProvider{staticRemoteProvider: staticRemoteProvider{files: []remoteSkillFile{{
		Path: skillManifestName, Contents: []byte(skillFile(name, "Remote "+name+".", "original\n")),
	}}}}
}

func auditEvictContent(t *testing.T, store *remoteSkillStore, record remoteSkillRecord) {
	t.Helper()
	root, err := store.contentRoot(record)
	if err != nil {
		t.Fatal(err)
	}
	// Delete only the resolved generation returned by the store, not the cache root.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if !record.fresh(time.Now()) {
		t.Fatal("fixture must retain a fresh timestamp after content eviction")
	}
}

func TestAuditRemoteMissingGenerationRecovery(t *testing.T) {
	for _, operation := range []string{"ensure", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			manager := newTestManager(t)
			store, ref, provider := manager.remoteStore, auditRemoteRef("alpha"), auditProvider("alpha")
			record, err := store.ensure(t.Context(), ref, provider)
			if err != nil {
				t.Fatal(err)
			}
			root, err := store.contentRoot(record)
			if err != nil {
				t.Fatal(err)
			}
			original := provider.files[0].Contents
			edited := bytes.Replace(original, []byte("original\n"), []byte("local edit\n"), 1)
			if err := store.savePatch(t.Context(), ref, filepath.Join(root, skillManifestName), sha256.Sum256(original), edited); err != nil {
				t.Fatal(err)
			}
			if disabled, err := store.toggleModelInvocation(t.Context(), ref); err != nil || !disabled {
				t.Fatalf("create override: disabled=%v, err=%v", disabled, err)
			}
			patchBefore, err := os.ReadFile(store.patchPath(ref))
			if err != nil {
				t.Fatal(err)
			}
			overrideBefore, err := os.ReadFile(store.overridePath(ref))
			if err != nil {
				t.Fatal(err)
			}
			auditEvictContent(t, store, record)
			if needed, err := store.needsRefresh(ref); err != nil || !needed {
				t.Fatalf("missing fresh generation needsRefresh=%v, err=%v", needed, err)
			}
			if operation == "ensure" {
				_, err = store.ensure(t.Context(), ref, provider)
			} else {
				err = store.refresh(t.Context(), record, provider)
			}
			if err != nil {
				t.Fatal(err)
			}
			if provider.calls != 2 {
				t.Fatalf("fetch calls=%d, want 2", provider.calls)
			}
			next := loadRemoteRecord(t, store, ref)
			if next.ref() != ref || next.Content == record.Content {
				t.Fatalf("recovered record lost identity or reused missing generation: %#v", next)
			}
			assertFile(t, store.patchPath(ref), string(patchBefore))
			assertFile(t, store.overridePath(ref), string(overrideBefore))
			var output bytes.Buffer
			if err := manager.getContext(t.Context(), t.TempDir(), ref.Name, "", &output); err != nil {
				t.Fatal(err)
			}
			if output.String() != string(edited) {
				t.Fatalf("recovered layered body=%q", output.String())
			}
			skill, err := manager.findSkill(t.TempDir(), ref.Name)
			if err != nil || !skill.DisableModelInvocation {
				t.Fatalf("recovered override not applied: %#v, err=%v", skill, err)
			}
		})
	}
}

func TestAuditRemoteMissingGenerationDoesNotBlockOtherSkills(t *testing.T) {
	manager := newTestManager(t)
	alpha, beta := auditRemoteRef("alpha"), auditRemoteRef("beta")
	record, err := manager.remoteStore.ensure(t.Context(), alpha, auditProvider("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	betaProvider := auditProvider("beta")
	if _, err := manager.remoteStore.ensure(t.Context(), beta, betaProvider); err != nil {
		t.Fatal(err)
	}
	auditEvictContent(t, manager.remoteStore, record)
	if _, err := manager.remoteStore.ensure(t.Context(), beta, betaProvider); err != nil {
		t.Fatal(err)
	}
	if betaProvider.calls != 1 {
		t.Fatal("healthy fresh generation was refetched")
	}
	project := t.TempDir()
	if _, err := manager.selectRemote(t.Context(), project, auditRemoteRef("gamma"), auditProvider("gamma"), false); err != nil {
		t.Fatalf("unrelated installation blocked by missing generation: %v", err)
	}
	if _, err := manager.findSkill(project, beta.Name); err != nil {
		t.Fatalf("healthy discovery blocked: %v", err)
	}
}

func TestAuditRemoteMissingGenerationSyncAndBackgroundRefresh(t *testing.T) {
	for _, operation := range []string{"sync", "background"} {
		t.Run(operation, func(t *testing.T) {
			gitLog := fakeGit(t, map[string]map[string]gitTestFile{"default": {
				"skills/alpha/SKILL.md": {contents: skillFile("alpha", "Remote alpha.", "original\n"), mode: 0o644},
			}})
			manager := newTestManager(t)
			manager.remote = newRemoteRegistry("")
			ref := auditRemoteRef("alpha")
			record, err := manager.remoteStore.ensure(t.Context(), ref, manager.remote)
			if err != nil {
				t.Fatal(err)
			}
			auditEvictContent(t, manager.remoteStore, record)
			project := t.TempDir()
			if operation == "sync" {
				if err := saveLock(project, testLock(map[string]bool{"alpha": true}, nil, map[string]remoteSkillRef{"alpha": ref})); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				err = manager.sync(t.Context(), project, &output)
				if err == nil {
					assertFile(t, filepath.Join(project, ".agents", "skills", "alpha", skillManifestName), wantPlaceholder("alpha", "Remote alpha."))
				}
			} else {
				err = refreshPersistedRemoteSkills(t.Context(), manager, testLoggerSink())
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gitCloneCount(t, gitLog); got != 2 {
				t.Fatalf("clones=%d, want 2", got)
			}
			loadRemoteRecord(t, manager.remoteStore, ref)
		})
	}
}

func TestAuditRemoteRemovalWithoutGeneration(t *testing.T) {
	for _, operation := range []string{"store", "manager"} {
		t.Run(operation, func(t *testing.T) {
			manager := newTestManager(t)
			project, ref := t.TempDir(), auditRemoteRef("alpha")
			if _, err := manager.selectRemote(t.Context(), project, ref, auditProvider("alpha"), false); err != nil {
				t.Fatal(err)
			}
			record := loadRemoteRecord(t, manager.remoteStore, ref)
			auditEvictContent(t, manager.remoteStore, record)
			var err error
			if operation == "store" {
				err = manager.remoteStore.remove(t.Context(), ref)
			} else {
				_, err = manager.uninstallRemote(t.Context(), project, ref.Name, ref.key())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(manager.remoteStore.root, "entries", ref.key()+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("metadata retained after removal: %v", err)
			}
			if operation == "manager" {
				value, err := loadLock(project)
				if err != nil {
					t.Fatal(err)
				}
				if _, exists := value.remote(ref.Name); exists {
					t.Fatal("uninstall retained remote selection")
				}
				for _, harness := range []string{".agents", ".claude"} {
					if _, err := os.Stat(filepath.Join(project, harness, "skills", ref.Name, skillManifestName)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("uninstall retained %s placeholder: %v", harness, err)
					}
				}
			}
		})
	}
}

func TestAuditRemoteUnsafeContentIsNotRecoverable(t *testing.T) {
	for _, attack := range []string{"traversal", "symlink escape", "dangling symlink escape"} {
		t.Run(attack, func(t *testing.T) {
			manager := newTestManager(t)
			store, ref, provider := manager.remoteStore, auditRemoteRef("alpha"), auditProvider("alpha")
			record, err := store.ensure(t.Context(), ref, provider)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if attack == "traversal" {
				record.Content = "../outside"
			} else {
				record.Content = "content/escape"
				target := outside
				if attack == "dangling symlink escape" {
					target = filepath.Join(outside, "absent")
				}
				if err := os.Symlink(target, filepath.Join(store.root, filepath.FromSlash(record.Content))); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(store.root, "entries", ref.key()+".json")
			if err := saveRemoteMetadataFile(path, record); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ensure(t.Context(), ref, provider); err == nil {
				t.Fatal("unsafe persisted content was treated as recoverable eviction")
			}
			if provider.calls != 1 {
				t.Fatal("unsafe persisted content triggered a fetch")
			}
			assertFile(t, path, string(before))
		})
	}
}

func TestAuditRemoteMissingContentDirectoryRecovery(t *testing.T) {
	manager := newTestManager(t)
	store, ref, provider := manager.remoteStore, auditRemoteRef("alpha"), auditProvider("alpha")
	record, err := store.ensure(t.Context(), ref, provider)
	if err != nil {
		t.Fatal(err)
	}
	contentDirectory := filepath.Join(store.root, "content")
	if err := os.RemoveAll(contentDirectory); err != nil {
		t.Fatal(err)
	}
	records, err := store.records()
	if err != nil || len(records) != 1 || records[0].ref() != ref {
		t.Fatalf("content directory eviction lost identity metadata: %#v, err=%v", records, err)
	}
	if _, err := store.ensure(t.Context(), ref, provider); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 {
		t.Fatalf("fetch calls=%d, want 2", provider.calls)
	}
	next := loadRemoteRecord(t, store, ref)
	if next.ref() != record.ref() || next.Content == record.Content {
		t.Fatalf("recovered directory lost identity or generation: %#v", next)
	}
}

type auditProviderFunc func(context.Context, remoteSkillRef) ([]remoteSkillFile, error)

func (fetch auditProviderFunc) fetchSkill(ctx context.Context, ref remoteSkillRef) ([]remoteSkillFile, error) {
	return fetch(ctx, ref)
}

func TestAuditRemoteFailedRefetchPreservesRetainedState(t *testing.T) {
	for _, operation := range []string{"ensure", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			manager := newTestManager(t)
			store, ref, provider := manager.remoteStore, auditRemoteRef("alpha"), auditProvider("alpha")
			record, err := store.ensure(t.Context(), ref, provider)
			if err != nil {
				t.Fatal(err)
			}
			root, err := store.contentRoot(record)
			if err != nil {
				t.Fatal(err)
			}
			original := provider.files[0].Contents
			edited := bytes.Replace(original, []byte("original\n"), []byte("local edit\n"), 1)
			if err := store.savePatch(t.Context(), ref, filepath.Join(root, skillManifestName), sha256.Sum256(original), edited); err != nil {
				t.Fatal(err)
			}
			if _, err := store.toggleModelInvocation(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(store.root, "entries", ref.key()+".json"), store.patchPath(ref), store.overridePath(ref)}
			before := make([][]byte, len(paths))
			for i, path := range paths {
				before[i], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			auditEvictContent(t, store, record)
			fetchErr := errors.New("upstream temporarily unavailable")
			failing := auditProviderFunc(func(context.Context, remoteSkillRef) ([]remoteSkillFile, error) {
				return nil, fetchErr
			})
			if operation == "ensure" {
				_, err = store.ensure(t.Context(), ref, failing)
			} else {
				err = store.refresh(t.Context(), record, failing)
			}
			if !errors.Is(err, fetchErr) {
				t.Fatalf("failed refetch error=%v, want upstream failure", err)
			}
			for i, path := range paths {
				assertFile(t, path, string(before[i]))
			}
			entries, err := os.ReadDir(filepath.Join(store.root, "content"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed refetch left content: %v, err=%v", entries, err)
			}
		})
	}
}

func TestAuditRemoteProviderCancellationDoesNotPublish(t *testing.T) {
	for _, result := range []string{"cancellation error", "files after cancellation"} {
		t.Run(result, func(t *testing.T) {
			manager := newTestManager(t)
			project, ref := t.TempDir(), auditRemoteRef("alpha")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			provider := auditProviderFunc(func(context.Context, remoteSkillRef) ([]remoteSkillFile, error) {
				calls++
				cancel()
				if result == "cancellation error" {
					return nil, context.Canceled
				}
				return auditProvider("alpha").files, nil
			})
			if _, err := manager.selectRemote(ctx, project, ref, provider, false); !errors.Is(err, context.Canceled) {
				t.Fatalf("provider cancellation error=%v", err)
			}
			if calls != 1 {
				t.Fatalf("provider calls=%d, want 1", calls)
			}
			value, err := loadLock(project)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := value.remote(ref.Name); exists {
				t.Fatal("provider cancellation persisted remote selection")
			}
			if _, exists := value.enabled(ref.Name); exists {
				t.Fatal("provider cancellation persisted enabled selection")
			}
			for _, directory := range []string{"entries", "content"} {
				entries, err := os.ReadDir(filepath.Join(manager.remoteStore.root, directory))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("provider cancellation published %s: %v", directory, entries)
				}
			}
			for _, harness := range []string{".agents", ".claude"} {
				if _, err := os.Stat(filepath.Join(project, harness, "skills", ref.Name, skillManifestName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("provider cancellation published %s placeholder: %v", harness, err)
				}
			}
		})
	}
}

func TestAuditPersistedCacheSerializedBound(t *testing.T) {
	const limit = 4 << 20
	for _, kind := range []string{"registry", "SkillsMP"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.json")
			makeCache := func(description string) any {
				if kind == "registry" {
					return remoteRegistryCache{SchemaRevision: remoteRegistrySchemaRevision, Topics: []remoteTopic{{Name: "Testing", Skills: []remoteSkill{{ID: "owner/repo/alpha", Name: "alpha", Description: description, Source: "owner/repo"}}}}}
				}
				return skillsMPCache{SchemaRevision: skillsMPSchemaRevision, Searches: map[string]skillsMPSearchResult{"alpha": {Skills: []skillsMPSkill{{ID: "alpha-id", Name: "alpha", Description: description}}}}}
			}
			save := func(value any) error {
				if kind == "registry" {
					return saveRemoteCache(path, value.(remoteRegistryCache))
				}
				return saveSkillsMPCache(path, value.(skillsMPCache))
			}
			load := func() error {
				if kind == "registry" {
					_, err := loadRemoteCache(path)
					return err
				}
				_, err := loadSkillsMPCache(path)
				return err
			}
			// Escaped bytes count toward persisted size, not just source string length.
			oversized := makeCache(strings.Repeat("<", limit/2))
			if err := save(makeCache("prior readable cache")); err != nil {
				t.Fatal(err)
			}
			prior, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := save(oversized); err == nil {
				t.Fatal("oversized serialized cache replaced prior cache")
			}
			assertFile(t, path, string(prior))
			aggregate := makeCache(strings.Repeat("x", 3<<20))
			if kind == "registry" {
				cache := aggregate.(remoteRegistryCache)
				cache.Topics = append(cache.Topics, cache.Topics[0])
				aggregate = cache
			} else {
				cache := aggregate.(skillsMPCache)
				cache.Searches["beta"] = cache.Searches["alpha"]
				aggregate = cache
			}
			if err := save(aggregate); err == nil {
				t.Fatal("aggregate of individually bounded entries exceeded cache limit")
			}
			assertFile(t, path, string(prior))
			if err := load(); err != nil {
				t.Fatalf("prior cache no longer readable: %v", err)
			}
			value := makeCache("")
			var encoded bytes.Buffer
			encoder := json.NewEncoder(&encoded)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(value); err != nil {
				t.Fatal(err)
			}
			// Stable ASCII padding makes the on-disk encoding exactly the limit.
			value = makeCache(strings.Repeat("x", limit-encoded.Len()))
			if err := save(value); err != nil {
				t.Fatalf("cache exactly at bound rejected: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() != limit {
				t.Fatalf("boundary fixture size: info=%v, err=%v", info, err)
			}
			if err := load(); err != nil {
				t.Fatalf("writer accepted cache reader rejects: %v", err)
			}
			boundary, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "registry" {
				cache := value.(remoteRegistryCache)
				cache.Topics[0].Skills[0].Description += "x"
				value = cache
			} else {
				cache := value.(skillsMPCache)
				result := cache.Searches["alpha"]
				result.Skills[0].Description += "x"
				cache.Searches["alpha"] = result
				value = cache
			}
			if err := save(value); err == nil {
				t.Fatal("writer accepted cache one byte over bound")
			}
			assertFile(t, path, string(boundary))
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString(" ")
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			if err := load(); err == nil {
				t.Fatal("reader accepted cache one byte over bound")
			}
		})
	}
}
