package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	remoteSkillSchemaRevision         = 1
	remoteSkillOverrideSchemaRevision = 1
	remoteSkillCacheTTL               = 3 * time.Hour
	remoteSkillMaxFiles               = 1024
	remoteSkillMaxBytes               = 16 << 20
	remoteSkillMetadataLimit          = 64 << 10
	remoteContentGracePeriod          = 2 * remoteSkillCacheTTL
	remoteSkillPatchDir               = ".remote-patches"
	skillsShProvider                  = "skills.sh"
	skillsMPProvider                  = "SkillsMP"
)

var errRemoteSkillPatch = errors.New("remote skill patch no longer applies")
var errRemoteSkillEditConflict = errors.New("remote skill changed while it was being edited")

const (
	remoteSkillPatchBaseHeader   = "# skills-mgr-base-sha256 "
	remoteSkillPatchResultHeader = "# skills-mgr-result-sha256 "
)

type remoteSkillRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Locator  string `json:"locator"`
}

func (r remoteSkillRef) key() string {
	sum := sha256.Sum256([]byte(r.Provider + "\x00" + r.ID))
	return hex.EncodeToString(sum[:])
}

func (r remoteSkillRef) validate() error {
	if r.Provider != skillsShProvider && r.Provider != skillsMPProvider {
		return fmt.Errorf("unsupported remote skill provider %q", r.Provider)
	}
	if r.ID == "" || len(r.ID) > 1024 {
		return fmt.Errorf("invalid %s skill identifier", r.Provider)
	}
	if !validSkillName(r.Name) {
		return fmt.Errorf("invalid remote skill name %q", r.Name)
	}
	if r.Locator == "" || len(r.Locator) > 4096 {
		return fmt.Errorf("invalid remote skill locator for %q", r.Name)
	}
	for _, character := range r.ID + r.Locator {
		if unicode.IsControl(character) {
			return fmt.Errorf("invalid remote skill metadata for %q", r.Name)
		}
	}
	return nil
}

type remoteSkillFile struct {
	Path     string
	Contents []byte
	Mode     fs.FileMode
}

type remoteSkillContentProvider interface {
	fetchSkill(
		ctx context.Context,
		ref remoteSkillRef,
	) ([]remoteSkillFile, error)
}

type remoteSkillRecord struct {
	SchemaRevision int       `json:"schemaRevision"`
	Provider       string    `json:"provider"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Locator        string    `json:"locator"`
	FetchedAt      time.Time `json:"fetchedAt"`
	Content        string    `json:"content"`
	// Kept outside the fetched record so refreshes cannot overwrite local policy.
	disableModelInvocationOverride *bool
}

type remoteSkillOverride struct {
	SchemaRevision         int   `json:"schemaRevision"`
	DisableModelInvocation *bool `json:"disableModelInvocation"`
}

func (r remoteSkillRecord) ref() remoteSkillRef {
	return remoteSkillRef{
		Provider: r.Provider,
		ID:       r.ID,
		Name:     r.Name,
		Locator:  r.Locator,
	}
}

func (m *manager) remoteSkillFrontmatter(ref remoteSkillRef) (string, error) {
	records, err := m.remoteStore.records()
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Provider != ref.Provider || record.ID != ref.ID {
			continue
		}
		if record.Name != ref.Name || record.Locator != ref.Locator {
			return "", fmt.Errorf("persisted remote skill identity changed")
		}
		skillPath := filepath.Join(
			m.remoteStore.root,
			filepath.FromSlash(record.Content),
			"SKILL.md",
		)
		skill, ok, err := parseSkill(skillPath)
		if err != nil {
			return "", err
		}
		if !ok || skill.Name != ref.Name {
			return "", fmt.Errorf("persisted remote skill %q is invalid", ref.Name)
		}
		file, err := os.Open(skillPath)
		if err != nil {
			return "", err
		}
		defer file.Close()
		frontmatter, _, status, err := readFrontmatter(file)
		if err != nil {
			return "", err
		}
		if status != frontmatterValid {
			return "", fmt.Errorf("persisted remote skill %q is invalid", ref.Name)
		}
		return frontmatter, nil
	}
	return "", fmt.Errorf("persisted remote skill %q is missing", ref.Name)
}

func (r remoteSkillRecord) fresh(now time.Time) bool {
	return now.Before(r.FetchedAt.Add(remoteSkillCacheTTL))
}

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

func (s *remoteSkillStore) overridePath(ref remoteSkillRef) string {
	return filepath.Join(s.root, "entries", ref.key()+".override")
}

func (s *remoteSkillStore) patchPath(ref remoteSkillRef) string {
	return filepath.Join(s.patchRoot, ref.key()+".patch")
}

func (s *remoteSkillStore) sidecars(ref remoteSkillRef) []struct {
	path string
	name string
} {
	return []struct {
		path string
		name string
	}{
		{path: s.overridePath(ref), name: "override"},
		{path: s.patchPath(ref), name: "patch"},
	}
}

func (s *remoteSkillStore) savePatch(
	ctx context.Context,
	ref remoteSkillRef,
	basePath string,
	expectedLayeredDigest [sha256.Size]byte,
	edited []byte,
) error {
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

	record, err := s.loadRecordLocked(ref.key())
	if err != nil {
		return err
	}
	if record.ref() != ref {
		return fmt.Errorf("persisted remote skill identity changed")
	}
	root, err := s.contentRoot(record)
	if err != nil {
		return err
	}
	currentPath := filepath.Join(root, "SKILL.md")
	if filepath.Clean(currentPath) != filepath.Clean(basePath) {
		return errRemoteSkillEditConflict
	}
	original, err := os.ReadFile(currentPath)
	if err != nil {
		return fmt.Errorf("read remote skill: %w", err)
	}
	currentLayered, err := s.applyPatch(ref, original)
	if err != nil {
		if errors.Is(err, errRemoteSkillPatch) {
			// Fall back to original content when patch no longer applies
			currentLayered = original
		} else {
			return err
		}
	}
	if sha256.Sum256(currentLayered) != expectedLayeredDigest {
		return errRemoteSkillEditConflict
	}

	patchText, err := makeRemoteSkillPatch(original, edited)
	if err != nil {
		return fmt.Errorf("create remote skill patch: %w", err)
	}
	path := s.patchPath(ref)
	if patchText == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove remote skill patch: %w", err)
		}
		return nil
	}
	baseDigest := sha256.Sum256(original)
	resultDigest := sha256.Sum256(edited)
	patch := fmt.Appendf(
		nil,
		"%s%x\n%s%x\n%s",
		remoteSkillPatchBaseHeader,
		baseDigest,
		remoteSkillPatchResultHeader,
		resultDigest,
		patchText,
	)
	if err := saveRemoteEntryFile(s.patchRoot, path, patch); err != nil {
		return fmt.Errorf("write remote skill patch: %w", err)
	}
	return nil
}

func (s *remoteSkillStore) applyPatch(
	ref remoteSkillRef,
	original []byte,
) ([]byte, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	path := s.patchPath(ref)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return original, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect remote skill patch: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: patch is not a regular file", errRemoteSkillPatch)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read remote skill patch: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: patch is empty", errRemoteSkillPatch)
	}

	baseHeader, remainder, hasResult := strings.Cut(string(data), "\n")
	resultHeader, patchText, hasPatch := strings.Cut(remainder, "\n")
	baseDigest, hasBaseHeader := strings.CutPrefix(baseHeader, remoteSkillPatchBaseHeader)
	resultDigest, hasResultHeader := strings.CutPrefix(
		resultHeader,
		remoteSkillPatchResultHeader,
	)
	if !hasResult || !hasPatch || !hasBaseHeader || !hasResultHeader || patchText == "" ||
		baseDigest == resultDigest {
		return nil, fmt.Errorf("%w: malformed patch", errRemoteSkillPatch)
	}
	baseSum := sha256.Sum256(original)
	if baseDigest != hex.EncodeToString(baseSum[:]) {
		return nil, errRemoteSkillPatch
	}
	patched, err := applyRemoteSkillPatch(original, patchText)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed patch", errRemoteSkillPatch)
	}
	resultSum := sha256.Sum256(patched)
	if resultDigest != hex.EncodeToString(resultSum[:]) {
		return nil, errRemoteSkillPatch
	}
	return patched, nil
}

func (s *remoteSkillStore) loadOverrideLocked(ref remoteSkillRef) (*bool, error) {
	path := s.overridePath(ref)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect remote skill override: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("remote skill override is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open remote skill override: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, remoteSkillMetadataLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read remote skill override: %w", err)
	}
	if len(data) > remoteSkillMetadataLimit {
		return nil, fmt.Errorf(
			"remote skill override exceeds %d bytes",
			remoteSkillMetadataLimit,
		)
	}
	var value remoteSkillOverride
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode remote skill override: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode remote skill override: unexpected data")
	}
	if value.SchemaRevision != remoteSkillOverrideSchemaRevision {
		return nil, fmt.Errorf(
			"unsupported remote skill override schema revision %d",
			value.SchemaRevision,
		)
	}
	if value.DisableModelInvocation == nil {
		return nil, fmt.Errorf("remote skill override is missing disableModelInvocation")
	}
	return value.DisableModelInvocation, nil
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
	data, err := io.ReadAll(io.LimitReader(file, remoteSkillMetadataLimit+1))
	if err != nil {
		return remoteSkillRecord{}, fmt.Errorf("read remote skill metadata: %w", err)
	}
	if len(data) > remoteSkillMetadataLimit {
		return remoteSkillRecord{}, fmt.Errorf(
			"remote skill metadata exceeds %d bytes",
			remoteSkillMetadataLimit,
		)
	}
	var record remoteSkillRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return remoteSkillRecord{}, fmt.Errorf("decode remote skill metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return remoteSkillRecord{}, fmt.Errorf("decode remote skill metadata: unexpected data")
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

type remoteToggleResult struct {
	Skill          string
	Enabled        bool
	Skills         []discoveredSkill
	Selected       map[string]bool
	RemoteSelected map[string]bool
}

type remoteUninstallResult struct {
	Skill           string
	Skills          []discoveredSkill
	Selected        map[string]bool
	GlobalSelected  map[string]bool
	ProjectSelected map[string]bool
}

func (m *manager) sync(
	ctx context.Context,
	project string,
	output io.Writer,
) (retErr error) {
	if m.remoteStore == nil {
		return fmt.Errorf("remote skill store is unavailable")
	}
	var placeholderUndos []func() error
	rollbackPlaceholders := func() error {
		rollbackErr := error(nil)
		for _, undo := range slices.Backward(placeholderUndos) {
			rollbackErr = errors.Join(rollbackErr, undo())
		}
		return rollbackErr
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, rollbackPlaceholders())
		}
	}()
	changePlaceholders := func(name, frontmatter string, enabled bool) error {
		undo, err := m.changeRemotePlaceholders(project, name, frontmatter, enabled)
		if err != nil {
			return err
		}
		if undo != nil {
			placeholderUndos = append(placeholderUndos, undo)
		}
		return nil
	}

	globalLock, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return err
	}
	projectLock, err := loadLock(project)
	if err != nil {
		return err
	}
	originalSkills := maps.Clone(projectLock.Skills)
	originalExpressions := maps.Clone(projectLock.Expressions)
	originalRemote := maps.Clone(projectLock.Remote)
	persistedRefs, err := m.persistedRemoteRefs()
	if err != nil {
		return err
	}
	metadataChanged, err := reconcileRemoteMetadata(
		globalLock,
		&projectLock,
		persistedRefs,
	)
	if err != nil {
		return fmt.Errorf("reconcile remote metadata: %w", err)
	}

	selected, _ := mergeSelectionLocks(globalLock, projectLock)
	discovered, err := m.skills(project)
	if err != nil {
		return err
	}
	discoveredByName := make(map[string]discoveredSkill, len(discovered))
	for _, skill := range discovered {
		discoveredByName[skill.Name] = skill
	}
	names := slices.Sorted(maps.Keys(projectLock.Remote))

	for _, name := range names {
		ref := projectLock.Remote[name]
		if !projectLock.Skills[name] {
			frontmatter, _ := m.remoteSkillFrontmatter(ref)
			if err := changePlaceholders(name, frontmatter, false); err != nil {
				return fmt.Errorf("sync remote skill %q: %w", name, err)
			}
		}
		if !selected[name] {
			continue
		}
		if ref.Name != name {
			return fmt.Errorf(
				"sync remote skill %q: selection name does not match remote name %q",
				name,
				ref.Name,
			)
		}
		if skill, ok := discoveredByName[name]; ok && skill.RemoteKey != ref.key() {
			persisted, isPersisted := persistedRefs[name]
			if !isPersisted || persisted != ref {
				return fmt.Errorf(
					"sync remote skill %q: skill name is already discovered from %s",
					name,
					skill.Source,
				)
			}
		}
		if err := ref.validate(); err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		provider := m.remoteContentProvider(ref.Provider)
		if _, err := m.remoteStore.ensure(ctx, ref, provider); err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		frontmatter, err := m.remoteSkillFrontmatter(ref)
		if err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		if projectLock.Skills[name] {
			if err := changePlaceholders(name, frontmatter, true); err != nil {
				return fmt.Errorf("sync remote skill %q: %w", name, err)
			}
		}
		if _, err := fmt.Fprintln(output, name); err != nil {
			return fmt.Errorf("report synchronized skill %q: %w", name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconcile remote metadata: %w", err)
	}
	err = updateLock(project, m.paths.selectionLocks, func(current *lock) (bool, error) {
		if !maps.Equal(current.Skills, originalSkills) ||
			!maps.Equal(current.Expressions, originalExpressions) ||
			!maps.Equal(current.Remote, originalRemote) {
			return false, fmt.Errorf("project selection changed during sync")
		}
		if !metadataChanged {
			return false, nil
		}
		maps.Copy(current.Remote, projectLock.Remote)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("persist synchronized remote metadata: %w", err)
	}
	return nil
}

func (m *manager) remoteContentProvider(provider string) remoteSkillContentProvider {
	switch provider {
	case skillsShProvider:
		return m.remote
	case skillsMPProvider:
		return m.skillsMP
	default:
		return nil
	}
}

func remoteSelectionFrom(
	skills []discoveredSkill,
	selected map[string]bool,
) map[string]bool {
	remote := make(map[string]bool)
	for _, skill := range skills {
		if skill.RemoteKey != "" && selected[skill.Name] {
			remote[skill.RemoteKey] = true
		}
	}
	return remote
}

func (m *manager) toggleRemote(
	ctx context.Context,
	project string,
	ref remoteSkillRef,
) (remoteToggleResult, error) {
	if m.remoteStore == nil {
		return remoteToggleResult{}, fmt.Errorf("remote skill store is unavailable")
	}
	if err := ref.validate(); err != nil {
		return remoteToggleResult{}, err
	}
	selectionLock, err := loadLock(m.lockDir(project))
	if err != nil {
		return remoteToggleResult{}, err
	}
	previousEnabled, previousExists := selectionLock.enabled(ref.Name)
	previousRemote, previousRemoteExists := selectionLock.Remote[ref.Name]
	rollbackSelection := func(expected bool) error {
		return m.updateSelectionLock(project, func(value *lock) (bool, error) {
			current, exists := value.enabled(ref.Name)
			if !exists || current.Boolean == nil ||
				*current.Boolean != expected ||
				value.Remote[ref.Name] != ref {
				return false, fmt.Errorf("remote selection changed during placeholder rollback")
			}
			if previousExists {
				value.setEnabled(ref.Name, previousEnabled)
			} else {
				value.deleteEnabled(ref.Name)
			}
			if previousRemoteExists {
				value.Remote[ref.Name] = previousRemote
			} else {
				delete(value.Remote, ref.Name)
			}
			return true, nil
		})
	}
	skills, err := m.skills(project)
	if err != nil {
		return remoteToggleResult{}, err
	}
	selection, err := m.selectionState(project, skills)
	if err != nil {
		return remoteToggleResult{}, err
	}
	selected := selection.selected
	key := ref.key()
	enabled := false
	for _, skill := range skills {
		if skill.Name != ref.Name {
			continue
		}
		if skill.RemoteKey != key {
			return remoteToggleResult{}, fmt.Errorf(
				"skill name %q is already discovered from %s",
				ref.Name,
				skill.Source,
			)
		}
		enabled = selected[skill.Name]
		break
	}
	if enabled {
		frontmatter, err := m.remoteSkillFrontmatter(ref)
		if err != nil {
			return remoteToggleResult{}, err
		}
		selected, err = m.setRemoteSelection(project, ref, false)
		if err != nil {
			return remoteToggleResult{}, err
		}
		if err := m.setRemotePlaceholders(project, ref.Name, frontmatter, false); err != nil {
			rollbackErr := rollbackSelection(false)
			return remoteToggleResult{}, errors.Join(err, rollbackErr)
		}
		return newRemoteToggleResult(ref.Name, false, skills, selected), nil
	}

	provider := m.remoteContentProvider(ref.Provider)
	if _, err := m.remoteStore.ensure(ctx, ref, provider); err != nil {
		return remoteToggleResult{}, err
	}
	skills, err = m.skills(project)
	if err != nil {
		return remoteToggleResult{}, err
	}
	found := false
	for _, skill := range skills {
		if skill.Name == ref.Name && skill.RemoteKey == key {
			found = true
			break
		}
	}
	if !found {
		return remoteToggleResult{}, fmt.Errorf(
			"persisted remote skill %q was not discovered",
			ref.Name,
		)
	}
	frontmatter, err := m.remoteSkillFrontmatter(ref)
	if err != nil {
		return remoteToggleResult{}, err
	}
	if err := m.setRemotePlaceholders(project, ref.Name, frontmatter, true); err != nil {
		return remoteToggleResult{}, err
	}
	selected, err = m.setRemoteSelection(project, ref, true)
	if err != nil {
		cleanupErr := m.setRemotePlaceholders(project, ref.Name, frontmatter, false)
		return remoteToggleResult{}, errors.Join(err, cleanupErr)
	}
	return newRemoteToggleResult(ref.Name, true, skills, selected), nil
}

func (m *manager) uninstallRemote(
	ctx context.Context,
	project string,
	name string,
	key string,
) (remoteUninstallResult, error) {
	if m.remoteStore == nil {
		return remoteUninstallResult{}, fmt.Errorf("remote skill store is unavailable")
	}
	ref, err := m.persistedRemoteRef(key, name)
	if err != nil {
		return remoteUninstallResult{}, err
	}
	if !m.global {
		global, err := loadLock(m.paths.globalLockDir)
		if err != nil {
			return remoteUninstallResult{}, err
		}
		_, globallyConfigured := global.Remote[name]
		_, legacyGlobalSelection := global.Skills[name]
		if globallyConfigured ||
			(global.SchemaRevision == legacyLockSchemaRevision && legacyGlobalSelection) {
			return remoteUninstallResult{}, fmt.Errorf(
				"remote skill %q is configured globally; uninstall it from the global TUI",
				name,
			)
		}
	}
	frontmatter, err := m.remoteSkillFrontmatter(ref)
	if err != nil {
		return remoteUninstallResult{}, err
	}
	skills, err := m.discoverSkills(project, key)
	if err != nil {
		return remoteUninstallResult{}, err
	}

	var currentSelected map[string]bool
	var previousEnabled enabledValue
	var previousExists bool
	var previousRemote remoteSkillRef
	var previousRemoteExists bool
	err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
		previousEnabled, previousExists = value.enabled(name)
		previousRemote, previousRemoteExists = value.Remote[name]
		value.deleteEnabled(name)
		delete(value.Remote, name)
		currentSelected = configuredSelections(*value)
		return previousExists || previousRemoteExists, nil
	})
	if err != nil {
		return remoteUninstallResult{}, err
	}

	rollbackSelection := func() error {
		return m.updateSelectionLock(project, func(value *lock) (bool, error) {
			if _, exists := value.enabled(name); exists {
				return false, fmt.Errorf("remote selection changed during uninstall rollback")
			}
			if _, exists := value.Remote[name]; exists {
				return false, fmt.Errorf("remote selection changed during uninstall rollback")
			}
			if previousExists {
				value.setEnabled(name, previousEnabled)
			}
			if previousRemoteExists {
				value.Remote[name] = previousRemote
			}
			return previousExists || previousRemoteExists, nil
		})
	}
	undoPlaceholders, err := m.changeRemotePlaceholders(project, name, frontmatter, false)
	if err != nil {
		return remoteUninstallResult{}, errors.Join(err, rollbackSelection())
	}
	rollbackPlaceholders := func() error {
		if undoPlaceholders == nil {
			return nil
		}
		return undoPlaceholders()
	}
	var globalSelected map[string]bool
	removeErr := updateLock(
		m.paths.globalLockDir,
		m.paths.selectionLocks,
		func(global *lock) (bool, error) {
			_, globallySelected := global.enabled(name)
			_, globallyConfigured := global.Remote[name]
			legacyGlobalSelection := global.SchemaRevision == legacyLockSchemaRevision &&
				globallySelected
			if !m.global && (globallyConfigured || legacyGlobalSelection) {
				return false, fmt.Errorf(
					"remote skill %q is configured globally; uninstall it from the global TUI",
					name,
				)
			}
			if m.global && (globallySelected || globallyConfigured) {
				return false, fmt.Errorf("remote selection changed during uninstall")
			}
			globalSelected = configuredSelections(*global)
			return false, m.remoteStore.remove(ctx, ref)
		},
	)
	if removeErr != nil {
		return remoteUninstallResult{}, errors.Join(
			removeErr,
			rollbackSelection(),
			rollbackPlaceholders(),
		)
	}

	selected := maps.Clone(globalSelected)
	var resultGlobalSelected, projectSelected map[string]bool
	if !m.global {
		resultGlobalSelected = globalSelected
		projectSelected = currentSelected
		maps.Copy(selected, projectSelected)
		for _, skill := range skills {
			if skillEnabled(selected, skill) {
				selected[skill.Name] = true
			}
		}
	}
	return remoteUninstallResult{
		Skill:           name,
		Skills:          skills,
		Selected:        selected,
		GlobalSelected:  resultGlobalSelected,
		ProjectSelected: projectSelected,
	}, nil
}

func newRemoteToggleResult(
	skill string,
	enabled bool,
	skills []discoveredSkill,
	selected map[string]bool,
) remoteToggleResult {
	return remoteToggleResult{
		Skill:          skill,
		Enabled:        enabled,
		Skills:         skills,
		Selected:       selected,
		RemoteSelected: remoteSelectionFrom(skills, selected),
	}
}
