package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
)

type projectEvidence struct {
	languages map[string]bool
	tooling   map[string]bool
	manifests []string
}

type projectEvidenceIndex struct {
	loadOnce  sync.Once
	evidence  projectEvidence
	root      string
	recursive bool
	loadErr   error
}

func newProjectEvidenceIndex(project string) *projectEvidenceIndex {
	recursive := true
	if home, err := os.UserHomeDir(); err == nil {
		// Home is a command context, not one project containing every nested
		// checkout and cache. Direct files still describe home itself.
		recursive = filepath.Clean(project) != filepath.Clean(home)
	}
	return &projectEvidenceIndex{
		evidence:  newProjectEvidence(),
		root:      project,
		recursive: recursive,
	}
}

func (index *projectEvidenceIndex) has(
	ctx context.Context,
	matches map[string]bool,
	name string,
) (bool, error) {
	if err := index.prepare(ctx); err != nil {
		return false, err
	}
	return matches[name], nil
}

func (index *projectEvidenceIndex) prepare(ctx context.Context) error {
	index.loadOnce.Do(func() {
		index.loadErr = index.load(ctx)
	})
	return index.loadErr
}

func (index *projectEvidenceIndex) manifestPaths(ctx context.Context) ([]string, error) {
	if err := index.prepare(ctx); err != nil {
		return nil, err
	}
	paths := slices.Clone(index.evidence.manifests)
	slices.Sort(paths)
	return paths, nil
}

func (index *projectEvidenceIndex) load(ctx context.Context) error {
	type readRequest struct {
		directory string
		relative  []string
		matcher   projectIgnoreMatcher
	}
	type readResult struct {
		request readRequest
		entries []os.DirEntry
		err     error
	}
	ignore, err := loadProjectIgnoreState(ctx, index.root)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(index.root)
	if err != nil {
		return err
	}
	rootRelative, err := filepath.Rel(ignore.root, root)
	if err != nil {
		return err
	}
	rootPath := projectIgnorePath(rootRelative)
	rootKey := filepath.ToSlash(filepath.Join(rootPath...))
	if len(rootPath) > 0 && ignore.matcher.match(rootPath, true) && !ignore.trackedDirs[rootKey] {
		return nil
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan readRequest)
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
				case request, ok := <-jobs:
					if !ok {
						return
					}
					entries, err := os.ReadDir(request.directory)
					select {
					case results <- readResult{request: request, entries: entries, err: err}:
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

	directories := []readRequest{{
		directory: root,
		relative:  rootPath,
		matcher:   ignore.matcher,
	}}
	active := 0
	for len(directories) > 0 || active > 0 {
		var next readRequest
		var send chan readRequest
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
					result.request.directory,
					result.err,
				)
			}
			matcher := result.request.matcher
			for _, entry := range result.entries {
				if entry.Name() != ".gitignore" {
					continue
				}
				data, err := os.ReadFile(filepath.Join(result.request.directory, entry.Name()))
				if err != nil {
					stopWorkers()
					return fmt.Errorf(
						"read project ignore file %s: %w",
						result.request.directory,
						err,
					)
				}
				matcher = matcher.append(string(data), result.request.relative)
				break
			}
			for _, entry := range result.entries {
				relative := slices.Concat(result.request.relative, []string{entry.Name()})
				key := filepath.ToSlash(filepath.Join(relative...))
				if entry.IsDir() {
					if !index.recursive {
						continue
					}
					switch entry.Name() {
					case ".git", "node_modules", "target":
						continue
					}
					if matcher.match(relative, true) && !ignore.trackedDirs[key] {
						continue
					}
					directories = append(
						directories,
						readRequest{
							directory: filepath.Join(result.request.directory, entry.Name()),
							relative:  relative,
							matcher:   matcher,
						},
					)
					continue
				}
				if !ignore.tracked[key] && matcher.match(relative, false) {
					continue
				}
				index.evidence.record(
					filepath.Join(result.request.directory, entry.Name()),
					entry.Name(),
				)
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

func (evidence *projectEvidence) record(path, name string) {
	recordLanguageFile(evidence.languages, name)
	recordToolingFile(evidence.tooling, name)
	switch name {
	case "go.mod", "Cargo.toml", "package.json":
		evidence.manifests = append(evidence.manifests, path)
	}
}
