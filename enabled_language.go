package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
)

const languageBuiltin = "lang"

func languageCallHandler(project string) interp.CallHandlerFunc {
	var index map[string]bool
	var indexErr error
	var once sync.Once
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != languageBuiltin {
			return args, nil
		}
		if len(args) != 2 {
			return nil, fmt.Errorf("%s: usage: %s <language>", languageBuiltin, languageBuiltin)
		}
		language, ok := canonicalLanguage(args[1])
		if !ok {
			return nil, fmt.Errorf("%s: unsupported language %q", languageBuiltin, args[1])
		}
		once.Do(func() {
			index, indexErr = loadLanguageIndex(ctx, project)
		})
		if indexErr != nil {
			return nil, fmt.Errorf("%s: %w", languageBuiltin, indexErr)
		}
		return []string{strconv.FormatBool(index[language])}, nil
	}
}

func canonicalLanguage(language string) (string, bool) {
	switch language {
	case "go", "rust", "node", "typescript", "javascript", "python", "c", "c++", "c#",
		"java", "lua", "vb", "php", "r", "ruby", "swift", "perl", "assembly", "shell",
		"bash", "postgres", "sql", "yaml", "json", "toml", "ini":
		return language, true
	case "ts":
		return "typescript", true
	case "js":
		return "javascript", true
	case "asm":
		return "assembly", true
	case "sh":
		return "shell", true
	default:
		return "", false
	}
}

func loadLanguageIndex(ctx context.Context, project string) (map[string]bool, error) {
	output, err := exec.CommandContext(
		ctx,
		"git",
		"-C",
		project,
		"ls-files",
		"-coz",
		"--exclude-standard",
	).Output()
	if err == nil {
		deletedOutput, deletedErr := exec.CommandContext(
			ctx,
			"git",
			"-C",
			project,
			"ls-files",
			"-dz",
		).Output()
		if deletedErr != nil {
			err = deletedErr
		} else {
			deleted := make(map[string]bool)
			for relative := range bytes.SplitSeq(deletedOutput, []byte{0}) {
				if len(relative) != 0 {
					deleted[string(relative)] = true
				}
			}
			index := make(map[string]bool)
			for relative := range bytes.SplitSeq(output, []byte{0}) {
				path := string(relative)
				if path == "" || deleted[path] || !filepath.IsLocal(path) || ignoredProjectPath(path) {
					continue
				}
				recordLanguageFile(index, filepath.Base(path))
			}
			return index, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return scanLanguageIndex(ctx, project)
}

func scanLanguageIndex(ctx context.Context, project string) (map[string]bool, error) {
	index := make(map[string]bool)
	directories := []string{project}
	for len(directories) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := directories[len(directories)-1]
		directories = directories[:len(directories)-1]
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read language directory %s: %w", directory, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules", "target":
					continue
				}
				directories = append(directories, filepath.Join(directory, entry.Name()))
				continue
			}
			recordLanguageFile(index, entry.Name())
		}
	}
	return index, nil
}

func recordLanguageFile(index map[string]bool, name string) {
	switch name {
	case "go.mod":
		index["go"] = true
	case "Cargo.toml":
		index["rust"] = true
	case "package.json":
		index["node"] = true
	case "tsconfig.json":
		index["typescript"] = true
	case "pyproject.toml", "requirements.txt", "Pipfile":
		index["python"] = true
	case "composer.json":
		index["php"] = true
	case "Gemfile":
		index["ruby"] = true
	case "Package.swift":
		index["swift"] = true
	case "cpanfile":
		index["perl"] = true
	case "postgresql.conf", "pg_hba.conf", "pg_ident.conf":
		index["postgres"] = true
	}

	extension := filepath.Ext(name)
	if extension == ".C" {
		index["c++"] = true
		return
	}
	switch strings.ToLower(extension) {
	case ".go":
		index["go"] = true
	case ".rs":
		index["rust"] = true
	case ".ts", ".tsx", ".mts", ".cts":
		index["typescript"] = true
	case ".js", ".jsx", ".mjs", ".cjs":
		index["javascript"] = true
	case ".py", ".pyw":
		index["python"] = true
	case ".c":
		index["c"] = true
	case ".cc", ".cpp", ".cxx", ".c++", ".hh", ".hpp", ".hxx":
		index["c++"] = true
	case ".cs", ".csproj":
		index["c#"] = true
	case ".java":
		index["java"] = true
	case ".lua":
		index["lua"] = true
	case ".vb", ".vbproj":
		index["vb"] = true
	case ".php":
		index["php"] = true
	case ".r", ".rmd", ".rproj":
		index["r"] = true
	case ".rb":
		index["ruby"] = true
	case ".swift":
		index["swift"] = true
	case ".pl", ".pm":
		index["perl"] = true
	case ".asm", ".s":
		index["assembly"] = true
	case ".sh":
		index["shell"] = true
	case ".bash":
		index["bash"] = true
	case ".psql":
		index["postgres"] = true
	case ".sql":
		index["sql"] = true
	case ".yaml", ".yml":
		index["yaml"] = true
	case ".json":
		index["json"] = true
	case ".toml":
		index["toml"] = true
	case ".ini":
		index["ini"] = true
	}
}
