package main

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"

	"io/fs"

	"os"
	"path/filepath"

	"strings"
	"sync"
	"syscall"
	"time"
)

type remoteSkillStore struct {
	root      string
	patchRoot string
	mu        sync.Mutex
}

func newRemoteSkillStore(root, patchRoot string) *remoteSkillStore {
	return &remoteSkillStore{root: root, patchRoot: patchRoot}
}

func (s *remoteSkillStore) ensure(
	ctx context.Context,
	ref remoteSkillRef,
	provider remoteSkillContentProvider,
) (remoteSkillRecord, error) {
	if err := ref.validate(); err != nil {
		return remoteSkillRecord{}, err
	}
	current, err := s.inspect(ctx, ref)
	if err != nil {
		return remoteSkillRecord{}, err
	}
	if current.SchemaRevision != 0 && current.fresh(time.Now()) {
		return current, nil
	}
	prepared, err := s.prepare(ctx, ref, provider)
	if err != nil {
		return remoteSkillRecord{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(filepath.Join(s.root, filepath.FromSlash(prepared.content)))
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.lockExclusive(ctx)
	if err != nil {
		return remoteSkillRecord{}, err
	}
	defer closeRemoteStoreLock(storeLock)

	records, err := s.recordsLocked()
	if err != nil {
		return remoteSkillRecord{}, err
	}
	current, err = findRemoteRecord(records, ref)
	if err != nil {
		return remoteSkillRecord{}, err
	}
	if current.SchemaRevision != 0 && current.fresh(time.Now()) {
		return current, nil
	}
	if current.SchemaRevision == 0 {
		err := os.Remove(s.overridePath(ref))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return remoteSkillRecord{}, fmt.Errorf(
				"remove stale remote skill override: %w",
				err,
			)
		}
	}
	record := prepared.record(ref)
	if err := s.saveRecordLocked(record); err != nil {
		return remoteSkillRecord{}, err
	}
	published = true
	s.cleanupContentLocked(prepared.completedAt)
	return record, nil
}

func (s *remoteSkillStore) needsRefresh(ref remoteSkillRef) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ref.validate(); err != nil {
		return false, err
	}
	records, err := s.recordsLocked()
	if err != nil {
		return false, err
	}
	current, err := findRemoteRecord(records, ref)
	if err != nil {
		return false, err
	}
	return current.SchemaRevision == 0 || !current.fresh(time.Now()), nil
}

func (s *remoteSkillStore) remove(ctx context.Context, ref remoteSkillRef) error {
	if err := ref.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	storeLock, err := s.lockExclusive(ctx)
	if err != nil {
		return err
	}
	defer closeRemoteStoreLock(storeLock)

	records, err := s.recordsLocked()
	if err != nil {
		return err
	}
	current, err := findRemoteRecord(records, ref)
	if err != nil {
		return err
	}
	if current.SchemaRevision == 0 {
		return fmt.Errorf("persisted remote skill %q is missing", ref.Name)
	}
	entries := filepath.Join(s.root, "entries")
	type stagedSidecar struct {
		original string
		staged   string
	}
	var staged []stagedSidecar
	rollbackStaged := func() error {
		var rollbackErr error
		for index := len(staged) - 1; index >= 0; index-- {
			rollbackErr = errors.Join(
				rollbackErr,
				os.Rename(staged[index].staged, staged[index].original),
			)
		}
		return rollbackErr
	}
	for _, sidecar := range s.sidecars(ref) {
		if _, err := os.Lstat(sidecar.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return errors.Join(
				fmt.Errorf("inspect remote skill %s: %w", sidecar.name, err),
				rollbackStaged(),
			)
		}
		temporary, err := os.CreateTemp(filepath.Dir(sidecar.path), ".remote-skill-remove-")
		if err != nil {
			return errors.Join(
				fmt.Errorf("stage remote skill %s removal: %w", sidecar.name, err),
				rollbackStaged(),
			)
		}
		stagedPath := temporary.Name()
		if err := errors.Join(temporary.Close(), os.Remove(stagedPath)); err != nil {
			return errors.Join(
				fmt.Errorf("stage remote skill %s removal: %w", sidecar.name, err),
				rollbackStaged(),
			)
		}
		if err := os.Rename(sidecar.path, stagedPath); err != nil {
			return errors.Join(
				fmt.Errorf("stage remote skill %s removal: %w", sidecar.name, err),
				rollbackStaged(),
			)
		}
		staged = append(staged, stagedSidecar{original: sidecar.path, staged: stagedPath})
	}

	path := filepath.Join(entries, ref.key()+".json")
	if err := os.Remove(path); err != nil {
		return errors.Join(
			fmt.Errorf("remove remote skill metadata: %w", err),
			rollbackStaged(),
		)
	}
	for _, sidecar := range staged {
		_ = os.RemoveAll(sidecar.staged)
	}
	return nil
}

func (s *remoteSkillStore) toggleModelInvocation(
	ctx context.Context,
	ref remoteSkillRef,
) (bool, error) {
	if err := ref.validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	storeLock, err := s.lockExclusive(ctx)
	if err != nil {
		return false, err
	}
	defer closeRemoteStoreLock(storeLock)

	record, err := s.loadRecordLocked(ref.key())
	if err != nil {
		return false, err
	}
	if record.ref() != ref {
		return false, fmt.Errorf("persisted remote skill identity changed")
	}
	override, err := s.loadOverrideLocked(ref)
	if err != nil {
		return false, err
	}
	var disabled bool
	if override != nil {
		disabled = *override
	} else {
		root, err := s.contentRoot(record)
		if err != nil {
			return false, err
		}
		skill, ok, err := parseSkill(filepath.Join(root, "SKILL.md"))
		if err != nil {
			return false, err
		}
		if !ok || skill.Name != ref.Name {
			return false, fmt.Errorf("persisted remote skill %q is invalid", ref.Name)
		}
		disabled = skill.DisableModelInvocation
	}
	disabled = !disabled
	value := remoteSkillOverride{
		SchemaRevision:         remoteSkillOverrideSchemaRevision,
		DisableModelInvocation: new(disabled),
	}
	entries := filepath.Join(s.root, "entries")
	if err := saveRemoteMetadataFile(entries, s.overridePath(ref), value); err != nil {
		return false, fmt.Errorf("write remote skill override: %w", err)
	}
	return disabled, nil
}

func (s *remoteSkillStore) refresh(
	ctx context.Context,
	record remoteSkillRecord,
	provider remoteSkillContentProvider,
) error {
	current, err := s.inspect(ctx, record.ref())
	if err != nil {
		return err
	}
	if current.SchemaRevision == 0 {
		return fmt.Errorf("persisted remote skill is missing")
	}
	if current.Provider != record.Provider || current.ID != record.ID {
		return fmt.Errorf("persisted remote skill identity changed during refresh")
	}
	if current.fresh(time.Now()) {
		return nil
	}
	ref := current.ref()
	prepared, err := s.prepare(ctx, ref, provider)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(filepath.Join(s.root, filepath.FromSlash(prepared.content)))
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.lockExclusive(ctx)
	if err != nil {
		return err
	}
	defer closeRemoteStoreLock(storeLock)
	current, err = s.loadRecordLocked(ref.key())
	if err != nil {
		return err
	}
	if current.Provider != ref.Provider || current.ID != ref.ID ||
		current.Name != ref.Name || current.Locator != ref.Locator {
		return fmt.Errorf("persisted remote skill identity changed during refresh")
	}
	if current.fresh(time.Now()) {
		return nil
	}
	next := prepared.record(ref)
	if err := s.saveRecordLocked(next); err != nil {
		return err
	}
	published = true
	s.cleanupContentLocked(prepared.completedAt)
	return nil
}

type preparedRemoteContent struct {
	content     string
	completedAt time.Time
}

func (p preparedRemoteContent) record(ref remoteSkillRef) remoteSkillRecord {
	return remoteSkillRecord{
		SchemaRevision: remoteSkillSchemaRevision,
		Provider:       ref.Provider,
		ID:             ref.ID,
		Name:           ref.Name,
		Locator:        ref.Locator,
		FetchedAt:      p.completedAt,
		Content:        p.content,
	}
}

func (s *remoteSkillStore) prepare(
	ctx context.Context,
	ref remoteSkillRef,
	provider remoteSkillContentProvider,
) (preparedRemoteContent, error) {
	if provider == nil {
		return preparedRemoteContent{}, fmt.Errorf(
			"%s content provider is unavailable",
			ref.Provider,
		)
	}
	files, err := provider.fetchSkill(ctx, ref)
	if err != nil {
		return preparedRemoteContent{}, fmt.Errorf("fetch remote skill %q: %w", ref.Name, err)
	}
	content, err := s.writeContent(ref, files)
	if err != nil {
		return preparedRemoteContent{}, err
	}
	return preparedRemoteContent{
		content:     content,
		completedAt: time.Now().UTC(),
	}, nil
}

func (s *remoteSkillStore) inspect(
	ctx context.Context,
	ref remoteSkillRef,
) (remoteSkillRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.lockExclusive(ctx)
	if err != nil {
		return remoteSkillRecord{}, err
	}
	defer closeRemoteStoreLock(storeLock)
	records, err := s.recordsLocked()
	if err != nil {
		return remoteSkillRecord{}, err
	}
	return findRemoteRecord(records, ref)
}

func (s *remoteSkillStore) lockExclusive(ctx context.Context) (*os.File, error) {
	if s == nil || s.root == "" {
		return nil, fmt.Errorf("remote skill store is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, fmt.Errorf("create remote skill store: %w", err)
	}
	lock, err := os.OpenFile(
		filepath.Join(s.root, ".lock"),
		os.O_CREATE|os.O_RDWR,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open remote skill store lock: %w", err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return lock, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = lock.Close()
				return nil, ctx.Err()
			case <-timer.C:
				continue
			}
		default:
			_ = lock.Close()
			return nil, fmt.Errorf("lock remote skill store: %w", err)
		}
	}
}

func closeRemoteStoreLock(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func (s *remoteSkillStore) cleanupContentLocked(now time.Time) {
	records, err := s.recordsLocked()
	if err != nil {
		return
	}
	referenced := make(map[string]struct{}, len(records))
	for _, record := range records {
		referenced[filepath.Base(filepath.FromSlash(record.Content))] = struct{}{}
	}
	contentRoot := filepath.Join(s.root, "content")
	entries, err := os.ReadDir(contentRoot)
	if err != nil {
		return
	}
	cutoff := now.Add(-remoteContentGracePeriod)
	for _, entry := range entries {
		if _, exists := referenced[entry.Name()]; exists {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(contentRoot, entry.Name()))
	}
}

func (s *remoteSkillStore) writeContent(
	ref remoteSkillRef,
	files []remoteSkillFile,
) (string, error) {
	if len(files) == 0 || len(files) > remoteSkillMaxFiles {
		return "", fmt.Errorf(
			"remote skill %q contains %d files; want 1..%d",
			ref.Name,
			len(files),
			remoteSkillMaxFiles,
		)
	}
	contentRoot := filepath.Join(s.root, "content")
	if err := os.MkdirAll(contentRoot, 0o700); err != nil {
		return "", fmt.Errorf("create remote skill content directory: %w", err)
	}
	temporary, err := os.MkdirTemp(contentRoot, "."+ref.key()+"-")
	if err != nil {
		return "", fmt.Errorf("create remote skill content: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temporary)
		}
	}()

	seen := make(map[string]struct{}, len(files))
	total := 0
	hasSkill := false
	hasAgents := false
	hasClaude := false
	hasClaudeAlias := false
	for _, file := range files {
		relative, err := validRemoteFilePath(file.Path)
		if err != nil {
			return "", fmt.Errorf("remote skill %q: %w", ref.Name, err)
		}
		if _, exists := seen[relative]; exists {
			return "", fmt.Errorf("remote skill %q has duplicate path %q", ref.Name, relative)
		}
		seen[relative] = struct{}{}
		isClaudeAlias := relative == "CLAUDE.md" &&
			file.Mode&fs.ModeSymlink != 0 && string(file.Contents) == "AGENTS.md"
		if file.Mode.Type() != 0 && !isClaudeAlias {
			return "", fmt.Errorf("remote skill %q has unsupported non-regular path %q", ref.Name, relative)
		}
		hasClaudeAlias = hasClaudeAlias || isClaudeAlias
		total += len(file.Contents)
		if total > remoteSkillMaxBytes {
			return "", fmt.Errorf(
				"remote skill %q exceeds %d bytes",
				ref.Name,
				remoteSkillMaxBytes,
			)
		}
		if relative == "SKILL.md" {
			hasSkill = true
		}
		if relative == "AGENTS.md" {
			hasAgents = true
		}
		if relative == "CLAUDE.md" {
			hasClaude = true
		}
		if isClaudeAlias {
			continue
		}
		destination := filepath.Join(temporary, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", fmt.Errorf("create remote skill directory: %w", err)
		}
		mode := fs.FileMode(0o600)
		if file.Mode&0o111 != 0 {
			mode = 0o700
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", fmt.Errorf("create remote skill file %q: %w", relative, err)
		}
		_, writeErr := output.Write(file.Contents)
		if writeErr == nil {
			writeErr = output.Sync()
		}
		writeErr = errors.Join(writeErr, output.Close())
		if writeErr != nil {
			return "", fmt.Errorf("write remote skill file %q: %w", relative, writeErr)
		}
	}
	if !hasSkill {
		return "", fmt.Errorf("remote skill %q is missing SKILL.md", ref.Name)
	}
	if hasClaudeAlias && !hasAgents {
		return "", fmt.Errorf("remote skill %q CLAUDE.md link is missing AGENTS.md", ref.Name)
	}
	parsed, ok, err := parseSkill(filepath.Join(temporary, "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("validate remote skill %q: %w", ref.Name, err)
	}
	if !ok {
		return "", fmt.Errorf("remote skill %q has invalid SKILL.md", ref.Name)
	}
	if parsed.Name != ref.Name {
		return "", fmt.Errorf(
			"remote skill name %q does not match SKILL.md name %q",
			ref.Name,
			parsed.Name,
		)
	}
	if hasAgents && (!hasClaude || hasClaudeAlias) {
		claudePath := filepath.Join(temporary, "CLAUDE.md")
		if err := os.Symlink("AGENTS.md", claudePath); err != nil {
			return "", fmt.Errorf("link remote skill CLAUDE.md to AGENTS.md: %w", err)
		}
	}

	final := filepath.Join(contentRoot, strings.TrimPrefix(filepath.Base(temporary), "."))
	if err := os.Rename(temporary, final); err != nil {
		return "", fmt.Errorf("publish remote skill content: %w", err)
	}
	cleanup = false
	relative, err := filepath.Rel(s.root, final)
	if err != nil {
		_ = os.RemoveAll(final)
		return "", fmt.Errorf("locate remote skill content: %w", err)
	}
	return filepath.ToSlash(relative), nil
}

func validRemoteFilePath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\\') {
		return "", fmt.Errorf("invalid file path %q", value)
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned != value || !filepath.IsLocal(filepath.FromSlash(cleaned)) ||
		cleaned == "." {
		return "", fmt.Errorf("unsafe file path %q", value)
	}
	return cleaned, nil
}

func (s *remoteSkillStore) saveRecordLocked(record remoteSkillRecord) error {
	entries := filepath.Join(s.root, "entries")
	path := filepath.Join(entries, record.ref().key()+".json")
	if err := saveRemoteMetadataFile(entries, path, record); err != nil {
		return fmt.Errorf("write remote skill metadata: %w", err)
	}
	return nil
}

func saveRemoteMetadataFile(entries, path string, value any) error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return saveRemoteEntryFile(entries, path, data.Bytes())
}

func saveRemoteEntryFile(directory, path string, data []byte) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create entry directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".remote-skill-")
	if err != nil {
		return fmt.Errorf("create entry: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	_, err = temporary.Write(data)
	if err == nil {
		err = temporary.Chmod(0o600)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	return err
}
