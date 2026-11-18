package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if err := runCompanion(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "skills-mgr:", err)
		os.Exit(1)
	}
}

func runCompanion(args []string) error {
	core := os.Getenv("_SKILLS_MGR_TUI_CORE")
	cache := os.Getenv("_SKILLS_MGR_TUI_CACHE")
	if !filepath.IsAbs(core) || !filepath.IsAbs(cache) ||
		(len(args) != 0 && (len(args) != 1 || args[0] != "-g")) {
		return fmt.Errorf("run skills-mgr to open the TUI")
	}
	project, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}
	return runTUI(&manager{tuiCore: core, cacheDir: cache, global: len(args) == 1}, project)
}
