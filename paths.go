package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type paths struct {
	userSkills     string
	managedSkills  string
	claudeSkills   string
	claudeSettings string
	claudePlugins  string
	grokSkills     string
	grokConfig     string
	grokCommand    string
	codexHome      string
	adminSkills    string
	managerHome    string
	globalLockDir  string
	legacyLockDir  string
	placeholderDir string
	selectionLocks string
	remoteRegistry string
	skillsMP       string
	remoteSkills   string
	daemonSocket   string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return paths{}, fmt.Errorf("find cache directory: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	socketDir := os.Getenv("XDG_RUNTIME_DIR")
	if socketDir == "" {
		socketDir = filepath.Join(cache, "skills-mgr")
	}
	managerHome := filepath.Join(home, ".skills-mgr")
	return paths{
		userSkills:     filepath.Join(home, ".agents", "skills"),
		managedSkills:  filepath.Join(managerHome, "skills"),
		claudeSkills:   filepath.Join(home, ".claude", "skills"),
		claudeSettings: filepath.Join(home, ".claude", "settings.json"),
		claudePlugins:  filepath.Join(home, ".claude", "plugins"),
		grokSkills:     filepath.Join(home, ".grok", "skills"),
		grokConfig:     filepath.Join(home, ".grok", "config.toml"),
		grokCommand:    "grok",
		codexHome:      codexHome,
		adminSkills:    "/etc/codex/skills",
		managerHome:    managerHome,
		// The global selection file lives in the manager home, while global
		// placeholders stay under $HOME because that is where the harnesses
		// look for .agents/skills and .claude/skills.
		globalLockDir:  managerHome,
		legacyLockDir:  home,
		placeholderDir: home,
		selectionLocks: filepath.Join(cache, "skills-mgr", "selection-locks"),
		remoteRegistry: filepath.Join(cache, "skills-mgr", "skills-sh.json"),
		skillsMP:       filepath.Join(cache, "skills-mgr", "skillsmp.json"),
		remoteSkills:   filepath.Join(cache, "skills-mgr", "remote-skills"),
		daemonSocket:   filepath.Join(socketDir, "skills-mgr.sock"),
	}, nil
}

// codexSkills and codexPluginCache are derived rather than stored so every
// paths value agrees on Codex's layout from codexHome alone.
func (p paths) codexSkills() string {
	return filepath.Join(p.codexHome, "skills")
}

func (p paths) codexPluginCache() string {
	return filepath.Join(p.codexHome, "plugins", "cache")
}

// projectSkills returns a project-local harness skills root, the layout twin of
// the $HOME roots this type already owns. harness is the dot-directory name,
// such as ".agents" or ".claude".
func (paths) projectSkills(project, harness string) string {
	return filepath.Join(project, harness, "skills")
}

// relocateGlobalLock moves a global selection file left in the legacy location
// into the manager home. It leaves the legacy file alone once the manager home
// holds one, so a downgrade cannot silently discard the newer selection.
func (p paths) relocateGlobalLock() error {
	if p.globalLockDir == p.legacyLockDir {
		return nil
	}
	current := filepath.Join(p.globalLockDir, lockName)
	if _, err := os.Lstat(current); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect global selection file %s: %w", current, err)
	}
	legacy := filepath.Join(p.legacyLockDir, lockName)
	info, err := os.Lstat(legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy global selection file %s: %w", legacy, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("legacy global selection file %s is not a regular file", legacy)
	}
	if err := os.MkdirAll(p.globalLockDir, 0o755); err != nil {
		return fmt.Errorf("create manager home %s: %w", p.globalLockDir, err)
	}
	if err := os.Rename(legacy, current); err != nil {
		return fmt.Errorf("move global selection file to %s: %w", current, err)
	}
	return nil
}
