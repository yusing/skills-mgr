package main

import (
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
	remoteSkillSchemaRevision = 1
	remoteSkillCacheTTL       = 3 * time.Hour
	remoteSkillMaxFiles       = 1024
	remoteSkillMaxBytes       = 16 << 20
	remoteSkillMetadataLimit  = 64 << 10
	remoteContentGracePeriod  = 2 * remoteSkillCacheTTL
	skillsShProvider          = "skills.sh"
	skillsMPProvider          = "SkillsMP"
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
	root string
	mu   sync.Mutex
}

func newRemoteSkillStore(root string) *remoteSkillStore {
	return &remoteSkillStore{root: root}
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
	if err := os.MkdirAll(entries, 0o700); err != nil {
		return fmt.Errorf("create remote skill metadata directory: %w", err)
	}
	path := filepath.Join(entries, record.ref().key()+".json")
	temporary, err := os.CreateTemp(entries, ".remote-skill-")
	if err != nil {
		return fmt.Errorf("create remote skill metadata: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(record)
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
	if err != nil {
		return fmt.Errorf("write remote skill metadata: %w", err)
	}
	return nil
}

func (s *remoteSkillStore) records() ([]remoteSkillRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordsLocked()
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
	decoder := json.NewDecoder(strings.NewReader(string(data)))
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

	selected := mergeSelections(globalLock.Skills, projectLock.Skills)
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
	previousEnabled, previousExists := selectionLock.Skills[ref.Name]
	previousRemote, previousRemoteExists := selectionLock.Remote[ref.Name]
	rollbackSelection := func(expected bool) error {
		return m.updateSelectionLock(project, func(value *lock) (bool, error) {
			if value.Skills[ref.Name] != expected || value.Remote[ref.Name] != ref {
				return false, fmt.Errorf("remote selection changed during placeholder rollback")
			}
			if previousExists {
				value.Skills[ref.Name] = previousEnabled
			} else {
				delete(value.Skills, ref.Name)
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
	selected, err := m.selection(project)
	if err != nil {
		return remoteToggleResult{}, err
	}
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
