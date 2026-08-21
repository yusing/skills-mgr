package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	recursive   bool
	err         error
}

func newProjectEvidenceIndex(project string) *projectEvidenceIndex {
	recursive := true
	if home, err := os.UserHomeDir(); err == nil {
		// Home is a command context, not one project containing every nested
		// checkout and cache. Direct files still describe home itself.
		recursive = filepath.Clean(project) != filepath.Clean(home)
	}
	return &projectEvidenceIndex{
		evidence:    newProjectEvidence(),
		directories: []string{project},
		recursive:   recursive,
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
	if len(index.directories) > 0 {
		index.err = index.load(ctx)
		if index.err != nil {
			return false, index.err
		}
	}
	return matches[name], nil
}

func (index *projectEvidenceIndex) load(ctx context.Context) error {
	type readResult struct {
		directory string
		entries   []os.DirEntry
		err       error
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	results := make(chan readResult)
	var workers sync.WaitGroup
	// Directory reads dominate exhaustive negative checks. Bound parallelism
	// so one list can overlap filesystem latency without unbounded goroutines.
	for range min(32, max(4, runtime.GOMAXPROCS(0)*2)) {
		workers.Go(func() {
			for {
				select {
				case <-workerCtx.Done():
					return
				case directory, ok := <-jobs:
					if !ok {
						return
					}
					entries, err := os.ReadDir(directory)
					select {
					case results <- readResult{directory: directory, entries: entries, err: err}:
					case <-workerCtx.Done():
						return
					}
				}
			}
		})
	}
	stopWorkers := func() {
		cancel()
		close(jobs)
		workers.Wait()
	}

	directories := index.directories
	index.directories = nil
	active := 0
	for len(directories) > 0 || active > 0 {
		var next string
		var send chan string
		if len(directories) > 0 {
			next = directories[0]
			send = jobs
		}
		select {
		case <-ctx.Done():
			stopWorkers()
			return ctx.Err()
		case send <- next:
			directories = directories[1:]
			active++
		case result := <-results:
			active--
			if errors.Is(result.err, os.ErrPermission) {
				continue
			}
			if result.err != nil {
				stopWorkers()
				return fmt.Errorf(
					"read project evidence directory %s: %w",
					result.directory,
					result.err,
				)
			}
			for _, entry := range result.entries {
				if entry.IsDir() {
					if !index.recursive {
						continue
					}
					switch entry.Name() {
					case ".git", "node_modules", "target":
						continue
					}
					directories = append(
						directories,
						filepath.Join(result.directory, entry.Name()),
					)
					continue
				}
				index.evidence.record(entry.Name())
			}
		}
	}

	close(jobs)
	workers.Wait()
	return ctx.Err()
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
