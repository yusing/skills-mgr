package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

const (
	claudePluginSource = "claude-plugin"
	grokPluginSource   = "grok-plugin"
	grokBundledSource  = "grok-bundled"
)

type claudeSettingsFile struct {
	EnabledPlugins map[string]bool `json:"enabledPlugins"`
}

type claudeInstalledPluginsFile struct {
	Plugins map[string][]struct {
		InstallPath string `json:"installPath"`
	} `json:"plugins"`
}

type grokInspectFile struct {
	Skills []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"source"`
		UserInvocable       bool   `json:"userInvocable"`
		Vendor              string `json:"vendor"`
		CompatibilityStatus string `json:"compatibilityStatus"`
	} `json:"skills"`
}

type grokConfigFile struct {
	Skills struct {
		Disabled []string `toml:"disabled"`
	} `toml:"skills"`
}

type grokConfigSnapshot struct {
	data []byte
	info os.FileInfo
}

// nativeSkills supplies harness-owned catalogs that are intentionally absent
// from manager.skills: Claude controls plugin status, while Grok owns its
// plugin and bundled skill metadata and selection policy.
func (m *manager) nativeSkills(project string) ([]discoveredSkill, error) {
	claude, err := m.claudePluginSkills()
	if err != nil {
		return nil, err
	}
	grok, err := m.grokNativeSkills(project)
	if err != nil {
		return nil, err
	}
	return append(claude, grok...), nil
}

func (m *manager) claudePluginSkills() ([]discoveredSkill, error) {
	settingsData, err := os.ReadFile(m.paths.claudeSettings)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Claude settings: %w", err)
	}
	var settings claudeSettingsFile
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		return nil, fmt.Errorf("decode Claude settings %s: %w", m.paths.claudeSettings, err)
	}

	installedPath := filepath.Join(m.paths.claudePlugins, "installed_plugins.json")
	installedData, err := os.ReadFile(installedPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Claude installed plugins: %w", err)
	}
	var installed claudeInstalledPluginsFile
	if err := json.Unmarshal(installedData, &installed); err != nil {
		return nil, fmt.Errorf("decode Claude installed plugins %s: %w", installedPath, err)
	}

	pluginIDs := slices.Sorted(maps.Keys(installed.Plugins))
	var skills []discoveredSkill
	seen := make(map[string]struct{})
	for _, pluginID := range pluginIDs {
		enabled := settings.EnabledPlugins[pluginID]
		for _, installation := range installed.Plugins[pluginID] {
			discovery := skillDiscovery{
				seenPaths: make(map[string]struct{}),
				seenNames: make(map[string]struct{}),
			}
			if err := discovery.discoverRoot(skillRoot{
				path: filepath.Join(installation.InstallPath, "skills"), source: claudePluginSource,
			}); err != nil {
				return nil, err
			}
			for _, skill := range discovery.skills {
				key := pluginID + "\x00" + skill.Path
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				skill.Plugin = pluginID
				skill.ExternalEnabled = enabled
				skills = append(skills, skill)
			}
		}
	}
	return skills, nil
}

func (m *manager) grokNativeSkills(project string) ([]discoveredSkill, error) {
	if m.paths.grokCommand == "" {
		return nil, nil
	}
	command := exec.Command(m.paths.grokCommand, "inspect", "--json")
	command.Dir = project
	output, err := command.Output()
	if err != nil {
		if startErr, ok := errors.AsType[*exec.Error](err); ok && errors.Is(startErr.Err, exec.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("run grok inspect --json: %w", err)
	}
	var inspected grokInspectFile
	if err := json.Unmarshal(output, &inspected); err != nil {
		return nil, fmt.Errorf("decode grok inspect --json: %w", err)
	}
	disabled, err := loadGrokDisabled(m.paths.grokConfig)
	if err != nil {
		return nil, err
	}
	disabledSet := make(map[string]struct{}, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = struct{}{}
	}

	var skills []discoveredSkill
	for _, inspectedSkill := range inspected.Skills {
		source := ""
		switch inspectedSkill.Source.Type {
		case "plugin":
			source = grokPluginSource
		case "bundled":
			source = grokBundledSource
		default:
			continue
		}
		enabled := true
		if _, found := disabledSet[inspectedSkill.Name]; found {
			enabled = false
		}
		skills = append(skills, discoveredSkill{
			Name:                inspectedSkill.Name,
			Description:         inspectedSkill.Description,
			Path:                inspectedSkill.Source.Path,
			Root:                filepath.Dir(inspectedSkill.Source.Path),
			EntryPath:           filepath.Dir(inspectedSkill.Source.Path),
			Source:              source,
			Vendor:              inspectedSkill.Vendor,
			UserInvocable:       inspectedSkill.UserInvocable,
			CompatibilityStatus: inspectedSkill.CompatibilityStatus,
			ExternalEnabled:     enabled,
		})
	}
	return skills, nil
}

func loadGrokDisabled(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Grok config %s: %w", path, err)
	}
	var config grokConfigFile
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("decode Grok config %s: %w", path, err)
	}
	return config.Skills.Disabled, nil
}

func (m *manager) toggleGrokSkill(name string) (bool, error) {
	if err := os.MkdirAll(m.paths.selectionLocks, 0o700); err != nil {
		return false, fmt.Errorf("create Grok config lock directory: %w", err)
	}
	// The separate lock path remains stable when config.toml is atomically
	// replaced and serializes the complete transaction across manager processes.
	lockPath := filepath.Join(m.paths.selectionLocks, "grok-config.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open Grok config lock: %w", err)
	}
	defer lockFile.Close()
	for {
		err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return false, fmt.Errorf("lock Grok config: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	path := m.paths.grokConfig
	snapshot, err := readGrokConfig(path)
	if err != nil {
		return false, err
	}
	var config grokConfigFile
	if snapshot.info != nil {
		if err := toml.Unmarshal(snapshot.data, &config); err != nil {
			return false, fmt.Errorf("decode Grok config %s: %w", path, err)
		}
	}
	enabled := slices.Contains(config.Skills.Disabled, name)
	if enabled {
		config.Skills.Disabled = slices.DeleteFunc(config.Skills.Disabled, func(disabled string) bool {
			return disabled == name
		})
	} else {
		config.Skills.Disabled = append(config.Skills.Disabled, name)
	}
	updated, err := replaceGrokDisabled(snapshot.data, config.Skills.Disabled)
	if err != nil {
		return false, fmt.Errorf("update Grok config %s: %w", path, err)
	}
	if err := saveGrokConfig(path, snapshot, updated); err != nil {
		return false, err
	}
	return enabled, nil
}

func readGrokConfig(path string) (grokConfigSnapshot, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return grokConfigSnapshot{}, nil
	}
	if err != nil {
		return grokConfigSnapshot{}, fmt.Errorf("read Grok config %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return grokConfigSnapshot{}, fmt.Errorf("inspect Grok config %s: %w", path, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return grokConfigSnapshot{}, fmt.Errorf("read Grok config %s: %w", path, err)
	}
	return grokConfigSnapshot{data: data, info: info}, nil
}

func replaceGrokDisabled(data []byte, disabled []string) ([]byte, error) {
	valueDocument, err := toml.Marshal(struct {
		Disabled []string `toml:"disabled"`
	}{Disabled: disabled})
	if err != nil {
		return nil, err
	}
	_, value, found := bytes.Cut(bytes.TrimSpace(valueDocument), []byte(" = "))
	if !found {
		return nil, errors.New("encode skills.disabled")
	}

	var parser unstable.Parser
	parser.Reset(data)
	var table []string
	skillsInsertAt := -1
	for parser.NextExpression() {
		expression := parser.Expression()
		switch expression.Kind {
		case unstable.Table:
			table = tomlNodeKey(expression)
			if slices.Equal(table, []string{"skills"}) {
				key := expression.Child()
				end := int(key.Raw.Offset + key.Raw.Length)
				if newline := bytes.IndexByte(data[end:], '\n'); newline >= 0 {
					end += newline + 1
				} else {
					end = len(data)
				}
				skillsInsertAt = end
			}
		case unstable.KeyValue:
			expressionKey := tomlNodeKey(expression)
			key := append(slices.Clone(table), expressionKey...)
			if !slices.Equal(key, []string{"skills", "disabled"}) {
				if len(table) == 0 && slices.Equal(expressionKey, []string{"skills"}) {
					inline := expression.Value()
					if inline.Kind != unstable.InlineTable {
						return nil, errors.New("skills must be a TOML table")
					}
					children := inline.Children()
					for children.Next() {
						child := children.Node()
						if child.Kind == unstable.KeyValue && slices.Equal(tomlNodeKey(child), []string{"disabled"}) {
							return replaceTOMLValue(data, child, value)
						}
					}
					start := int(expression.Raw.Offset)
					end := start + int(expression.Raw.Length)
					closeOffset := bytes.LastIndexByte(data[start:end], '}')
					if closeOffset < 0 {
						return nil, errors.New("locate inline skills table")
					}
					insertAt := start + closeOffset
					separator := []byte("disabled = ")
					if len(bytes.TrimSpace(data[int(inline.Raw.Offset)+1:insertAt])) > 0 {
						separator = []byte(", disabled = ")
					}
					return slices.Concat(data[:insertAt], separator, value, data[insertAt:]), nil
				}
				continue
			}
			return replaceTOMLValue(data, expression, value)
		}
	}
	if err := parser.Error(); err != nil {
		return nil, err
	}
	if skillsInsertAt >= 0 {
		setting := slices.Concat([]byte("disabled = "), value, []byte("\n"))
		if skillsInsertAt > 0 && data[skillsInsertAt-1] != '\n' {
			setting = append([]byte("\n"), setting...)
		}
		return slices.Concat(data[:skillsInsertAt], setting, data[skillsInsertAt:]), nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(slices.Clone(data), '\n')
	}
	if len(data) > 0 {
		data = append(data, '\n')
	}
	return slices.Concat(data, []byte("[skills]\ndisabled = "), value, []byte("\n")), nil
}

func replaceTOMLValue(data []byte, expression *unstable.Node, value []byte) ([]byte, error) {
	start := int(expression.Raw.Offset)
	end := start + int(expression.Raw.Length)
	equals := bytes.IndexByte(data[start:end], '=')
	if equals < 0 {
		return nil, errors.New("locate TOML value")
	}
	start += equals + 1
	for start < end && (data[start] == ' ' || data[start] == '\t') {
		start++
	}
	return slices.Concat(data[:start], value, data[end:]), nil
}

func tomlNodeKey(node *unstable.Node) []string {
	var key []string
	iterator := node.Key()
	for iterator.Next() {
		key = append(key, string(iterator.Node().Data))
	}
	return key
}

func saveGrokConfig(path string, before grokConfigSnapshot, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Grok config directory: %w", err)
	}
	mode := os.FileMode(0o600)
	if before.info != nil {
		mode = before.info.Mode().Perm()
	}
	temporary, err := os.CreateTemp(directory, ".config.toml-")
	if err != nil {
		return fmt.Errorf("create Grok config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err = temporary.Chmod(mode)
	if err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err == nil {
		// Atomic rename prevents partial files, but this comparison prevents a
		// stale toggle from replacing edits made by Grok or another process.
		unchanged, checkErr := grokConfigUnchanged(path, before)
		switch {
		case checkErr != nil:
			err = checkErr
		case !unchanged:
			err = errors.New("Grok config changed while updating")
		default:
			err = os.Rename(temporaryPath, path)
		}
	}
	if err != nil {
		return fmt.Errorf("write Grok config %s: %w", path, err)
	}
	return nil
}

func grokConfigUnchanged(path string, before grokConfigSnapshot) (bool, error) {
	current, err := readGrokConfig(path)
	if err != nil {
		return false, err
	}
	if before.info == nil || current.info == nil {
		return before.info == nil && current.info == nil, nil
	}
	return os.SameFile(before.info, current.info) &&
		before.info.Mode().Perm() == current.info.Mode().Perm() &&
		bytes.Equal(before.data, current.data), nil
}
