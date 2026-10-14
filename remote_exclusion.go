package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// remoteSkillExclusion is a sorted, deduplicated set of path.Match patterns
// relative to a remote skill directory.
type remoteSkillExclusion []string

func newRemoteSkillExclusion(patterns []string) (remoteSkillExclusion, error) {
	for _, pattern := range patterns {
		if pattern == "" || strings.HasPrefix(pattern, "/") ||
			strings.ContainsFunc(pattern, unicode.IsControl) {
			return nil, fmt.Errorf("invalid exclude pattern %q", pattern)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", pattern, err)
		}
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	exclusion := slices.Clone(patterns)
	slices.Sort(exclusion)
	return slices.Compact(exclusion), nil
}

// excludes reports whether a pattern matches relative or one of its parent
// directories, so a directory pattern drops everything below it.
func (e remoteSkillExclusion) excludes(relative string) bool {
	for candidate := relative; candidate != "." && candidate != "/"; candidate = path.Dir(candidate) {
		for _, pattern := range e {
			if matched, _ := path.Match(pattern, candidate); matched {
				return true
			}
		}
	}
	return false
}

func (s *remoteSkillStore) exclusionPath(ref remoteSkillRef) string {
	return filepath.Join(s.patchRoot, ref.key()+".exclude")
}

func (s *remoteSkillStore) loadExclusion(ref remoteSkillRef) (remoteSkillExclusion, error) {
	path := s.exclusionPath(ref)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect remote skill exclusion: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("remote skill exclusion is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open remote skill exclusion: %w", err)
	}
	defer file.Close()
	data, err := readBoundedFile(file, remoteSkillMetadataLimit)
	if err != nil {
		return nil, fmt.Errorf("read remote skill exclusion: %w", err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	exclusion, err := newRemoteSkillExclusion(strings.Split(text, "\n"))
	if err != nil {
		return nil, fmt.Errorf("remote skill exclusion: %w", err)
	}
	return exclusion, nil
}

// replaceExclusion stores exclusion as the identity's patterns, removing the
// sidecar when it is empty, and returns a function that restores the
// previous sidecar.
func (s *remoteSkillStore) replaceExclusion(
	ctx context.Context,
	ref remoteSkillRef,
	exclusion remoteSkillExclusion,
) (func() error, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.lockExclusive(ctx)
	if err != nil {
		return nil, err
	}
	defer closeRemoteStoreLock(storeLock)

	path := s.exclusionPath(ref)
	previous, err := os.ReadFile(path)
	previousExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read remote skill exclusion: %w", err)
	}
	if err := writeExclusionFile(path, exclusion); err != nil {
		return nil, err
	}
	return func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		storeLock, err := s.lockExclusive(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
		defer closeRemoteStoreLock(storeLock)
		if previousExists {
			return writeAtomicFile(path, "remote skill exclusion", previous)
		}
		return writeExclusionFile(path, nil)
	}, nil
}

func writeExclusionFile(path string, exclusion remoteSkillExclusion) error {
	if len(exclusion) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove remote skill exclusion: %w", err)
		}
		return nil
	}
	data := []byte(strings.Join(exclusion, "\n") + "\n")
	return writeAtomicFile(path, "remote skill exclusion", data)
}
