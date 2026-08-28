package main

import (
	"errors"
	"fmt"

	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (m *manager) remoteSkillFrontmatter(ref remoteSkillRef) (string, error) {
	records, err := m.remoteStore.records()
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Provider != ref.Provider || record.ID != ref.ID {
			continue
		}
		return m.recordFrontmatter(record, ref)
	}
	return "", fmt.Errorf("persisted remote skill %q is missing", ref.Name)
}

// recordFrontmatter reads a persisted remote skill's frontmatter from a record
// the caller already holds. Callers iterating many skills use this so each one
// costs a single manifest read instead of a full store scan.
func (m *manager) recordFrontmatter(
	record remoteSkillRecord,
	ref remoteSkillRef,
) (string, error) {
	if record.Name != ref.Name || record.Locator != ref.Locator {
		return "", fmt.Errorf("persisted remote skill identity changed")
	}
	root, err := m.remoteStore.contentRoot(record)
	if err != nil {
		return "", err
	}
	file, err := os.Open(filepath.Join(root, skillManifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("persisted remote skill %q is invalid", ref.Name)
		}
		return "", err
	}
	defer file.Close()
	frontmatter, _, status, err := readFrontmatter(file)
	if err != nil {
		return "", err
	}
	skill, ok := skillFromFrontmatter(frontmatter)
	if status != frontmatterValid || !ok || skill.Name != ref.Name {
		return "", fmt.Errorf("persisted remote skill %q is invalid", ref.Name)
	}
	return frontmatter, nil
}

func findRemoteRecord(
	records []remoteSkillRecord,
	ref remoteSkillRef,
) (remoteSkillRecord, error) {
	var current remoteSkillRecord
	for _, record := range records {
		switch {
		case record.Provider == ref.Provider && record.ID == ref.ID:
			if record.Name != ref.Name || record.Locator != ref.Locator {
				return remoteSkillRecord{}, fmt.Errorf(
					"remote skill %s:%s identity conflicts with persisted reference",
					ref.Provider,
					ref.ID,
				)
			}
			current = record
		case record.Name == ref.Name:
			return remoteSkillRecord{}, fmt.Errorf(
				"remote skill name %q is already persisted from %s",
				ref.Name,
				record.Provider,
			)
		}
	}
	return current, nil
}

func (s *remoteSkillStore) records() ([]remoteSkillRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordsLocked()
}

func (s *remoteSkillStore) recordsForDiscovery(
	excludedRemoteKey string,
) ([]remoteSkillRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.recordsLocked()
	if err != nil {
		return nil, err
	}
	filtered := records[:0]
	for _, record := range records {
		if record.ref().key() == excludedRemoteKey {
			continue
		}
		override, err := s.loadOverrideLocked(record.ref())
		if err != nil {
			return nil, err
		}
		record.disableModelInvocationOverride = override
		filtered = append(filtered, record)
	}
	return filtered, nil
}

func (s *remoteSkillStore) recordsLocked() ([]remoteSkillRecord, error) {
	if s == nil || s.root == "" {
		return nil, nil
	}
	entries := filepath.Join(s.root, "entries")
	items, err := os.ReadDir(entries)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read remote skill metadata: %w", err)
	}
	records := make([]remoteSkillRecord, 0, len(items))
	for _, item := range items {
		if item.IsDir() || filepath.Ext(item.Name()) != ".json" {
			continue
		}
		key := strings.TrimSuffix(item.Name(), ".json")
		record, err := s.loadRecordLocked(key)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b remoteSkillRecord) int {
		if order := strings.Compare(a.Provider, b.Provider); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return records, nil
}

func (s *remoteSkillStore) loadRecordLocked(key string) (remoteSkillRecord, error) {
	path := filepath.Join(s.root, "entries", key+".json")
	file, err := os.Open(path)
	if err != nil {
		return remoteSkillRecord{}, fmt.Errorf("open remote skill metadata: %w", err)
	}
	defer file.Close()
	data, err := readBoundedFile(file, remoteSkillMetadataLimit)
	if err != nil {
		return remoteSkillRecord{}, fmt.Errorf("read remote skill metadata: %w", err)
	}
	var record remoteSkillRecord
	if err := decodeStrictJSON(data, &record); err != nil {
		return remoteSkillRecord{}, fmt.Errorf("decode remote skill metadata: %w", err)
	}
	if record.SchemaRevision != remoteSkillSchemaRevision {
		return remoteSkillRecord{}, fmt.Errorf(
			"unsupported remote skill schema revision %d",
			record.SchemaRevision,
		)
	}
	if err := record.ref().validate(); err != nil {
		return remoteSkillRecord{}, err
	}
	if record.ref().key() != key || record.FetchedAt.IsZero() {
		return remoteSkillRecord{}, fmt.Errorf("remote skill metadata identity is invalid")
	}
	if _, err := s.contentRoot(record); err != nil {
		return remoteSkillRecord{}, err
	}
	return record, nil
}

func (s *remoteSkillStore) contentRoot(record remoteSkillRecord) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(record.Content)) {
		return "", fmt.Errorf("remote skill content path is unsafe")
	}
	root := filepath.Join(s.root, filepath.FromSlash(record.Content))
	resolvedStore, err := filepath.EvalSymlinks(filepath.Join(s.root, "content"))
	if err != nil {
		return "", fmt.Errorf("resolve remote skill content store: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve remote skill content: %w", err)
	}
	relative, err := filepath.Rel(resolvedStore, resolvedRoot)
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("remote skill content escapes its store")
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("remote skill content is missing")
	}
	return resolvedRoot, nil
}
