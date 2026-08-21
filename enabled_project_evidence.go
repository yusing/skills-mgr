package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type projectEvidence struct {
	languages map[string]bool
	tooling   map[string]bool
}

type projectEvidenceIndex struct {
	mu          sync.Mutex
	evidence    projectEvidence
	directories []string
	err         error
}

func newProjectEvidenceIndex(project string) *projectEvidenceIndex {
	return &projectEvidenceIndex{
		evidence:    newProjectEvidence(),
		directories: []string{project},
	}
}

func (index *projectEvidenceIndex) has(
	ctx context.Context,
	matches map[string]bool,
	name string,
) (bool, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	if matches[name] {
		return true, nil
	}
	if index.err != nil {
		return false, index.err
	}
	for len(index.directories) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		directory := index.directories[len(index.directories)-1]
		index.directories = index.directories[:len(index.directories)-1]
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			index.err = fmt.Errorf("read project evidence directory %s: %w", directory, err)
			return false, index.err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules", "target":
					continue
				}
				index.directories = append(
					index.directories,
					filepath.Join(directory, entry.Name()),
				)
				continue
			}
			index.evidence.record(entry.Name())
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if matches[name] {
			return true, nil
		}
	}
	return false, nil
}

func newProjectEvidence() projectEvidence {
	return projectEvidence{
		languages: make(map[string]bool),
		tooling:   make(map[string]bool),
	}
}

func (evidence projectEvidence) record(name string) {
	recordLanguageFile(evidence.languages, name)
	recordToolingFile(evidence.tooling, name)
}
