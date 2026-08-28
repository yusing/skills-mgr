package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/format/gitignore"
	gitstorage "github.com/go-git/go-git/v6/storage/filesystem"
)

const projectIgnoreMatchEnd = "\x00skills-mgr-ignore-match-end"

// Adapted from ../git-agent/internal/ignore/matcher.go:10:112 Matcher.
// Git rules apply to each path prefix. A descendant cannot be re-included
// while one of its parent directories remains excluded.
type projectIgnoreMatcher struct {
	patterns []projectIgnorePattern
}

type projectIgnorePattern struct {
	parsed             gitignore.Pattern
	exact              gitignore.Pattern
	trailingDoubleStar gitignore.Pattern
	base               []string
	simpleExact        bool
	directoryOnly      bool
}

func (m projectIgnoreMatcher) append(text string, base []string) projectIgnoreMatcher {
	m.patterns = slices.Clone(m.patterns)
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized := trimProjectIgnoreTrailingSpaces(line)
		exact := strings.TrimSuffix(strings.TrimPrefix(normalized, "!"), "/")
		entry := projectIgnorePattern{
			parsed:        gitignore.ParsePattern(normalized, base),
			base:          slices.Clone(base),
			simpleExact:   !strings.Contains(exact, "/"),
			directoryOnly: strings.HasSuffix(strings.TrimPrefix(normalized, "!"), "/"),
		}
		switch {
		case entry.simpleExact:
		case strings.HasSuffix(exact, "/**"):
			// A trailing /** matches entries below its prefix, not the prefix
			// directory itself. Match the real endpoint as a file to avoid the
			// parser's directory stand-in for an omitted descendant.
			entry.trailingDoubleStar = gitignore.ParsePattern(
				strings.TrimSuffix(normalized, "/"),
				base,
			)
		default:
			entry.exact = gitignore.ParsePattern(
				strings.TrimSuffix(normalized, "/")+"/"+projectIgnoreMatchEnd,
				base,
			)
		}
		m.patterns = append(m.patterns, entry)
	}
	return m
}

func (m projectIgnoreMatcher) match(path []string, isDir bool) bool {
	ignored := false
	for end := 1; end <= len(path); end++ {
		prefix := path[:end]
		prefixIsDir := end < len(path) || isDir
		if matched, found := m.matchExact(prefix, prefixIsDir); found {
			ignored = matched == gitignore.Exclude
		}
		if ignored && end < len(path) {
			return true
		}
	}
	return ignored
}

func (m projectIgnoreMatcher) matchExact(
	path []string,
	isDir bool,
) (gitignore.MatchResult, bool) {
	for _, pattern := range slices.Backward(m.patterns) {
		if result := pattern.matchExact(path, isDir); result != gitignore.NoMatch {
			return result, true
		}
	}
	return gitignore.NoMatch, false
}

func (p projectIgnorePattern) matchExact(path []string, isDir bool) gitignore.MatchResult {
	if p.directoryOnly && !isDir {
		return gitignore.NoMatch
	}
	if p.simpleExact {
		if len(path) <= len(p.base) || !slices.Equal(path[:len(p.base)], p.base) {
			return gitignore.NoMatch
		}
		direct := slices.Concat(p.base, path[len(path)-1:])
		return p.parsed.Match(direct, isDir)
	}
	if p.trailingDoubleStar != nil {
		return p.trailingDoubleStar.Match(path, false)
	}
	if p.exact == nil {
		return gitignore.NoMatch
	}
	exactPath := slices.Concat(path, []string{projectIgnoreMatchEnd})
	return p.exact.Match(exactPath, false)
}

func trimProjectIgnoreTrailingSpaces(line string) string {
	for strings.HasSuffix(line, " ") {
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

type projectIgnoreState struct {
	root        string
	matcher     projectIgnoreMatcher
	tracked     map[string]bool
	trackedDirs map[string]bool
}

func loadProjectIgnoreState(ctx context.Context, project string) (projectIgnoreState, error) {
	project, err := filepath.Abs(project)
	if err != nil {
		return projectIgnoreState{}, err
	}
	state := projectIgnoreState{
		root:        project,
		tracked:     make(map[string]bool),
		trackedDirs: make(map[string]bool),
	}

	worktreeRoot := project
	found := false
	for directory := project; ; directory = filepath.Dir(directory) {
		if err := ctx.Err(); err != nil {
			return projectIgnoreState{}, err
		}
		if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
			worktreeRoot = directory
			found = true
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return projectIgnoreState{}, err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	if !found {
		return state, nil
	}
	repository, err := git.PlainOpen(worktreeRoot)
	if err != nil {
		// Git metadata only refines filesystem evidence. A stale or malformed
		// marker must not prevent filters from treating the project as standalone.
		return state, nil //nolint:nilerr // Fall back to project-rooted evidence.
	}
	index, err := repository.Storer.Index()
	if err != nil {
		_ = repository.Close()
		return state, nil //nolint:nilerr // Fall back to project-rooted evidence.
	}
	var repositoryExclude []byte
	if storage, ok := repository.Storer.(*gitstorage.Storage); ok {
		file, openErr := storage.Filesystem().Open(filepath.Join("info", "exclude"))
		switch {
		case openErr == nil:
			repositoryExclude, err = io.ReadAll(file)
			err = errors.Join(err, file.Close())
		case errors.Is(openErr, fs.ErrNotExist):
		default:
			err = openErr
		}
	}
	if closeErr := repository.Close(); err != nil || closeErr != nil {
		return state, nil //nolint:nilerr // Discard incomplete optional Git metadata.
	}
	state.root = worktreeRoot
	state.matcher = state.matcher.append(string(repositoryExclude), nil)
	projectRelative, err := filepath.Rel(worktreeRoot, project)
	if err != nil {
		return projectIgnoreState{}, err
	}
	projectBase := projectIgnorePath(projectRelative)
	projectPrefix := filepath.ToSlash(filepath.Join(projectBase...))
	if projectPrefix != "" {
		projectPrefix += "/"
	}
	for _, entry := range index.Entries {
		path := filepath.ToSlash(entry.Name)
		if projectPrefix != "" && !strings.HasPrefix(path, projectPrefix) {
			continue
		}
		state.tracked[path] = true
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			state.trackedDirs[parent] = true
		}
	}

	for depth := range projectBase {
		if err := ctx.Err(); err != nil {
			return projectIgnoreState{}, err
		}
		base := projectBase[:depth]
		path := filepath.Join(append([]string{worktreeRoot}, base...)...)
		data, err := os.ReadFile(filepath.Join(path, ".gitignore"))
		if err == nil {
			state.matcher = state.matcher.append(string(data), base)
			continue
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		return projectIgnoreState{}, fmt.Errorf("read project ignore file %s: %w", path, err)
	}
	return state, nil
}

func projectIgnorePath(path string) []string {
	path = strings.Trim(filepath.ToSlash(path), "/")
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, "/")
}
