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

	"os"
	"path/filepath"

	"strings"
)

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
	if err := writeAtomicFile(path, "entry", patch); err != nil {
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
	data, err := readBoundedFile(file, remoteSkillMetadataLimit)
	if err != nil {
		return nil, fmt.Errorf("read remote skill override: %w", err)
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
