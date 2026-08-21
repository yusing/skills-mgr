package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
)

const languageBuiltin = "lang"

type languageIndex struct {
	mu          sync.Mutex
	languages   map[string]bool
	directories []string
	err         error
}

func languageCallHandler(project string) interp.CallHandlerFunc {
	index := &languageIndex{
		languages:   make(map[string]bool),
		directories: []string{project},
	}
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
		found, err := index.has(ctx, language)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", languageBuiltin, err)
		}
		return []string{strconv.FormatBool(found)}, nil
	}
}

func canonicalLanguage(language string) (string, bool) {
	switch language {
	case "go", "rust", "node", "typescript", "tsx", "javascript", "jsx", "html", "css",
		"python", "c", "c++", "c#", "java", "lua", "vb", "php", "r", "ruby", "swift",
		"perl", "assembly", "shell", "bash", "postgres", "sql", "yaml", "json", "toml", "ini":
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

func (index *languageIndex) has(ctx context.Context, language string) (bool, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.languages[language] {
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
			index.err = fmt.Errorf("read language directory %s: %w", directory, err)
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
			recordLanguageFile(index.languages, entry.Name())
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if index.languages[language] {
			return true, nil
		}
	}
	return false, nil
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
	case ".ts", ".mts", ".cts":
		index["typescript"] = true
	case ".tsx":
		index["typescript"] = true
		index["tsx"] = true
	case ".js", ".mjs", ".cjs":
		index["javascript"] = true
	case ".jsx":
		index["javascript"] = true
		index["jsx"] = true
	case ".html", ".htm":
		index["html"] = true
	case ".css":
		index["css"] = true
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
