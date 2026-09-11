package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func printInfo(out io.Writer, p paths, project string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	home = filepath.Clean(home)
	pathStatus := func(path string) string {
		_, statErr := os.Stat(path)
		if runtime.GOOS != "windows" {
			if path == home {
				path = "~"
			} else if relative, ok := strings.CutPrefix(path, strings.TrimSuffix(home, "/")+"/"); ok {
				path = "~/" + relative
			}
		}
		if os.IsNotExist(statErr) {
			path += " [MISSING]"
		}
		return path
	}
	_, err = fmt.Fprintf(out, `Metadata:
  Project selection: %s
  Global selection: %s
  skills.sh registry: %s
  SkillsMP registry: %s
Skills:
  Project shared: %s
  User shared: %s
  Managed: %s
  Project Claude: %s
  User Claude: %s
  Project Grok: %s
  User Grok: %s
  Project Codex: %s
  User Codex: %s
  Admin: %s
  Claude plugins: %s
  Codex plugin cache: %s
Remote skills:
  Store: %s
  Local patches: %s
`,
		pathStatus(filepath.Join(project, lockName)), pathStatus(filepath.Join(p.globalLockDir, lockName)),
		pathStatus(p.remoteRegistry), pathStatus(p.skillsMP),
		pathStatus(p.projectSkills(project, ".agents")), pathStatus(p.userSkills), pathStatus(p.managedSkills),
		pathStatus(p.projectSkills(project, ".claude")), pathStatus(p.claudeSkills),
		pathStatus(p.projectSkills(project, ".grok")), pathStatus(p.grokSkills),
		pathStatus(p.projectSkills(project, ".codex")), pathStatus(p.codexSkills()), pathStatus(p.adminSkills),
		pathStatus(p.claudePlugins), pathStatus(p.codexPluginCache()),
		pathStatus(p.remoteSkills), pathStatus(filepath.Join(p.managedSkills, remoteSkillPatchDir)),
	)
	return err
}
