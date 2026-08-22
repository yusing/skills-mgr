package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNativeSkillsLoadClaudePluginsAndGrokInspect(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()

	pluginRoot := filepath.Join(manager.paths.claudePlugins, "cache", "market", "sample", "1.0.0")
	writeSkill(t, filepath.Join(pluginRoot, "skills", "claude-tool"), "claude-tool")
	writeJSONFile(t, manager.paths.claudeSettings, claudeSettingsFile{
		EnabledPlugins: map[string]bool{"sample@market": false},
	})
	writeJSONFile(t, filepath.Join(manager.paths.claudePlugins, "installed_plugins.json"), claudeInstalledPluginsFile{
		Plugins: map[string][]struct {
			InstallPath string `json:"installPath"`
		}{"sample@market": {{InstallPath: pluginRoot}}},
	})

	writeFile(t, manager.paths.grokConfig, "[skills]\ndisabled = [\"grok-plugin\"]\n")
	manager.paths.grokCommand = writeGrokInspectCommand(t, `{
  "skills": [
    {
      "name": "grok-builtin",
      "description": "Bundled by Grok.",
      "source": {"type": "bundled", "path": "/opt/grok/grok-builtin/SKILL.md"},
      "userInvocable": true,
      "vendor": "xai",
      "compatibilityStatus": "compatible"
    },
    {
      "name": "grok-plugin",
      "description": "Provided by a Grok plugin.",
      "source": {"type": "plugin", "path": "/opt/grok/grok-plugin/SKILL.md"},
      "userInvocable": false,
      "vendor": "example",
      "compatibilityStatus": null
    },
    {
      "name": "grok-user",
      "description": "Already discovered from the user root.",
      "source": {"type": "user", "path": "/home/test/.grok/skills/grok-user/SKILL.md"},
      "userInvocable": true
    }
  ]
}`)

	skills, err := manager.nativeSkills(project)
	if err != nil {
		t.Fatal(err)
	}
	if names := discoveredSkillNames(skills); !slices.Equal(names, []string{"claude-tool", "grok-builtin", "grok-plugin"}) {
		t.Fatalf("native skills = %q", names)
	}

	claude := findDiscoveredSkill(t, skills, "claude-tool")
	if claude.Source != claudePluginSource || claude.Plugin != "sample@market" ||
		claude.ExternalEnabled {
		t.Fatalf("Claude plugin metadata = %#v", claude)
	}
	bundled := findDiscoveredSkill(t, skills, "grok-builtin")
	if bundled.Source != grokBundledSource || !bundled.ExternalEnabled ||
		bundled.Vendor != "xai" || !bundled.UserInvocable ||
		bundled.CompatibilityStatus != "compatible" {
		t.Fatalf("Grok bundled metadata = %#v", bundled)
	}
	plugin := findDiscoveredSkill(t, skills, "grok-plugin")
	if plugin.Source != grokPluginSource || plugin.ExternalEnabled {
		t.Fatalf("Grok plugin metadata = %#v", plugin)
	}
}

func TestToggleGrokSkillOnlyUpdatesGlobalDisabledSetting(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	original := "# keep top\n[skills]\npaths = [\"/skills\"]\n# keep skill comment\ndisabled = [\"alpha\", \"other\"]\nserver_skill_dirs = []\n\n[unrelated]\nvalue = 7\n"
	writeFile(t, manager.paths.grokConfig, original)

	enabled, err := manager.toggleGrokSkill("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("enabling a disabled Grok skill returned false")
	}
	disabled, err := loadGrokDisabled(manager.paths.grokConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(disabled, []string{"other"}) {
		data, _ := os.ReadFile(manager.paths.grokConfig)
		t.Fatalf("disabled after enabling alpha = %q:\n%s", disabled, data)
	}
	data, err := os.ReadFile(manager.paths.grokConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"# keep top", "paths = [\"/skills\"]", "# keep skill comment", "server_skill_dirs = []", "[unrelated]", "value = 7"} {
		if !bytes.Contains(data, []byte(preserved)) {
			t.Errorf("Grok config lost %q:\n%s", preserved, data)
		}
	}
	if _, err := os.Stat(filepath.Join(project, lockName)); !os.IsNotExist(err) {
		t.Fatalf("project selection file was touched: %v", err)
	}

	enabled, err = manager.toggleGrokSkill("beta")
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("disabling an enabled Grok skill returned true")
	}
	disabled, err = loadGrokDisabled(manager.paths.grokConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(disabled, []string{"other", "beta"}) {
		t.Fatalf("disabled after disabling beta = %q", disabled)
	}
}

func TestSaveGrokConfigRejectsConcurrentChange(t *testing.T) {
	manager := newTestManager(t)
	original := []byte("[skills]\ndisabled = []\n[model]\nname = \"first\"\n")
	writeFile(t, manager.paths.grokConfig, string(original))
	snapshot, err := readGrokConfig(manager.paths.grokConfig)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := replaceGrokDisabled(original, []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	concurrent := "[skills]\ndisabled = []\n[model]\nname = \"newer\"\n"
	writeFile(t, manager.paths.grokConfig, concurrent)

	err = saveGrokConfig(manager.paths.grokConfig, snapshot, updated)
	if err == nil || !strings.Contains(err.Error(), "Grok config changed while updating") {
		t.Fatalf("error = %v", err)
	}
	data, readErr := os.ReadFile(manager.paths.grokConfig)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != concurrent {
		t.Fatalf("concurrent config changed to %q", data)
	}
}

func TestConcurrentGrokTogglesPreserveBothUpdates(t *testing.T) {
	manager := newTestManager(t)
	writeFile(t, manager.paths.grokConfig, "[skills]\ndisabled = []\n")
	start := make(chan struct{})
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		wait.Go(func() {
			<-start
			_, err := manager.toggleGrokSkill(name)
			errors <- err
		})
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	disabled, err := loadGrokDisabled(manager.paths.grokConfig)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(disabled)
	if !slices.Equal(disabled, []string{"alpha", "beta"}) {
		t.Fatalf("disabled after concurrent toggles = %q", disabled)
	}
}

func TestToggleGrokSkillAddsMissingDisabledSetting(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		want   []string
	}{
		{name: "missing skills table", config: "[unrelated]\nvalue = true\n", want: []string{"alpha"}},
		{name: "empty inline skills table", config: "skills = {}\n[unrelated]\nvalue = true\n", want: []string{"alpha"}},
		{name: "existing inline setting", config: "skills = { disabled = [\"other\"] }\n", want: []string{"other", "alpha"}},
		{name: "skills table without trailing newline", config: "[skills]", want: []string{"alpha"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newTestManager(t)
			writeFile(t, manager.paths.grokConfig, test.config)

			enabled, err := manager.toggleGrokSkill("alpha")
			if err != nil {
				t.Fatal(err)
			}
			if enabled {
				t.Fatal("new disabled entry reported enabled")
			}
			disabled, err := loadGrokDisabled(manager.paths.grokConfig)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(disabled, test.want) {
				t.Fatalf("disabled = %q", disabled)
			}
			data, err := os.ReadFile(manager.paths.grokConfig)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "[unrelated]\nvalue = true") && strings.Contains(test.config, "[unrelated]") {
				t.Fatalf("unrelated config changed:\n%s", data)
			}
		})
	}
}

func TestNativeSkillsStayOutOfList(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	pluginRoot := filepath.Join(manager.paths.claudePlugins, "cache", "market", "sample", "1")
	writeSkill(t, filepath.Join(pluginRoot, "skills", "claude-native"), "claude-native")
	writeJSONFile(t, manager.paths.claudeSettings, claudeSettingsFile{
		EnabledPlugins: map[string]bool{"sample@market": true},
	})
	writeJSONFile(t, filepath.Join(manager.paths.claudePlugins, "installed_plugins.json"), claudeInstalledPluginsFile{
		Plugins: map[string][]struct {
			InstallPath string `json:"installPath"`
		}{"sample@market": {{InstallPath: pluginRoot}}},
	})
	manager.paths.grokCommand = writeGrokInspectCommand(t, `{
  "skills": [{
    "name": "grok-native",
    "description": "Bundled.",
    "source": {"type": "bundled", "path": "/opt/grok/native/SKILL.md"},
    "userInvocable": true
  }]
}`)
	writeFile(t, manager.paths.grokConfig, "[skills]\ndisabled = [\"grok-native\"]\n")

	native, err := manager.nativeSkills(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(native) != 2 {
		t.Fatalf("native TUI catalog = %#v", native)
	}
	for _, harness := range []listHarness{listHarnessClaude, listHarnessGrok, listHarnessCodex} {
		var output bytes.Buffer
		if err := manager.listContext(t.Context(), project, &output, harness); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), "claude-native") || strings.Contains(output.String(), "grok-native") {
			t.Fatalf("native skills leaked into list for harness %d:\n%s", harness, output.String())
		}
	}
}

func TestClaudeAndGrokNestedSourceTabsAndControls(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeFile(t, manager.paths.grokConfig, "[skills]\ndisabled = [\"grok-native\"]\n")
	current := model{
		manager: manager,
		project: project,
		allSkills: []discoveredSkill{
			{Name: "grok-user", Source: "grok"},
			{Name: "grok-native", Source: grokBundledSource, ExternalEnabled: false, Vendor: "xai"},
			{Name: "claude-user", Source: "claude"},
			{Name: "claude-native", Source: claudePluginSource, ExternalEnabled: true, Plugin: "sample@market"},
		},
		selected: map[string]bool{"grok-user": true, "claude-user": true},
		width:    160,
		height:   16,
	}

	current.tab = grokTab
	current.applyCatalog()
	if view := current.View(); !strings.Contains(view, "[User]") || !strings.Contains(view, "grok-user") || strings.Contains(view, "grok-native") {
		t.Fatalf("Grok user subtab:\n%s", view)
	}
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	current = updated.(model)
	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	current = updated.(model)
	if current.sourceSubtabs[grokTab] != bundledSourceSubtab {
		t.Fatalf("Grok subtab = %d, want Bundled", current.sourceSubtabs[grokTab])
	}
	view := current.View()
	if !strings.Contains(view, "[Bundled]") || !strings.Contains(view, "grok-native") ||
		!strings.Contains(view, "[disabled]") || !strings.Contains(view, "(bundled)") {
		t.Fatalf("Grok bundled subtab:\n%s", view)
	}
	for _, key := range []string{"i", "m", "e"} {
		updated, command := current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		blocked := updated.(model)
		if command != nil || !strings.Contains(blocked.status, "use Space to change its global status") {
			t.Fatalf("Grok %q command = %v, status = %q", key, command, blocked.status)
		}
	}
	updated, command := current.Update(tea.KeyMsg{Type: tea.KeySpace})
	current = updated.(model)
	if command == nil || !current.busy {
		t.Fatal("Grok Space did not start the global status update")
	}
	updated, _ = current.Update(command())
	current = updated.(model)
	if current.busy || !strings.Contains(current.status, "enabled grok-native in Grok") {
		t.Fatalf("Grok toggle status = %q, busy = %t", current.status, current.busy)
	}

	current.tab = claudeTab
	current.sourceSubtabs[claudeTab] = pluginSourceSubtab
	current.cursor = 0
	current.applyCatalog()
	view = current.View()
	if !strings.Contains(view, "[Plugin]") || !strings.Contains(view, "claude-native") ||
		!strings.Contains(view, "[enabled]") || !strings.Contains(view, "plugin status display only") {
		t.Fatalf("Claude plugin subtab:\n%s", view)
	}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeySpace},
		{Type: tea.KeyRunes, Runes: []rune{'i'}},
		{Type: tea.KeyRunes, Runes: []rune{'m'}},
		{Type: tea.KeyRunes, Runes: []rune{'e'}},
	} {
		updated, command = current.Update(key)
		blocked := updated.(model)
		if command != nil || !strings.Contains(blocked.status, "display only") {
			t.Fatalf("Claude %q command = %v, status = %q", key.String(), command, blocked.status)
		}
	}
}

func TestGrokInspectRejectsInvalidJSON(t *testing.T) {
	manager := newTestManager(t)
	manager.paths.grokCommand = writeGrokInspectCommand(t, `{not-json}`)

	_, err := manager.grokNativeSkills(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "decode grok inspect --json") {
		t.Fatalf("error = %v", err)
	}
}

func TestToggleGrokSkillLeavesMalformedConfigUntouched(t *testing.T) {
	manager := newTestManager(t)
	original := "[skills\ndisabled = [\"alpha\"]\n"
	writeFile(t, manager.paths.grokConfig, original)

	_, err := manager.toggleGrokSkill("alpha")
	if err == nil || !strings.Contains(err.Error(), "decode Grok config") {
		t.Fatalf("error = %v", err)
	}
	data, readErr := os.ReadFile(manager.paths.grokConfig)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Fatalf("malformed config changed to %q", data)
	}
}

func writeGrokInspectCommand(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok")
	script := "#!/bin/sh\n" +
		"test \"$1\" = inspect && test \"$2\" = --json || exit 2\n" +
		"cat <<'SKILLS_MGR_JSON'\n" + output + "\nSKILLS_MGR_JSON\n"
	writeFile(t, path, script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}

func discoveredSkillNames(skills []discoveredSkill) []string {
	names := make([]string, len(skills))
	for index, skill := range skills {
		names[index] = skill.Name
	}
	slices.Sort(names)
	return names
}

func findDiscoveredSkill(t *testing.T, skills []discoveredSkill, name string) discoveredSkill {
	t.Helper()
	for _, skill := range skills {
		if skill.Name == name {
			return skill
		}
	}
	t.Fatalf("skill %q not found in %#v", name, skills)
	return discoveredSkill{}
}
