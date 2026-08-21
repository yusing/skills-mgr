package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	lockName                   = ".skills-mgr.json"
	legacyLockSchemaRevision   = 1
	previousLockSchemaRevision = 2
	lockSchemaRevision         = 3
)

type enabledValue struct {
	Boolean    *bool
	Expression string
}

func (v enabledValue) MarshalJSON() ([]byte, error) {
	switch {
	case v.Boolean != nil && v.Expression == "":
		return json.Marshal(*v.Boolean)
	case v.Boolean == nil && strings.TrimSpace(v.Expression) != "":
		return marshalJSONUnescaped(v.Expression)
	default:
		return nil, fmt.Errorf("enabled must be a boolean or non-empty string")
	}
}

func marshalJSONUnescaped(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func (v *enabledValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "true" || trimmed == "false" {
		var boolean bool
		if err := json.Unmarshal(data, &boolean); err != nil {
			return fmt.Errorf("decode enabled boolean: %w", err)
		}
		*v = enabledValue{Boolean: new(boolean)}
		return nil
	}
	var expression string
	if err := json.Unmarshal(data, &expression); err != nil {
		return fmt.Errorf("enabled must be a boolean or non-empty string")
	}
	if strings.TrimSpace(expression) == "" {
		return fmt.Errorf("enabled expression is empty")
	}
	*v = enabledValue{Expression: expression}
	return nil
}

type skillSelection struct {
	Enabled *enabledValue   `json:"enabled,omitempty"`
	Remote  *remoteSkillRef `json:"remote,omitempty"`
}

type previousSkillSelection struct {
	Enabled *bool           `json:"enabled,omitempty"`
	Remote  *remoteSkillRef `json:"remote,omitempty"`
}

type decodedLockFile struct {
	SchemaRevision int                        `json:"schema_revision"`
	Skills         map[string]*skillSelection `json:"skills"`
}

type previousDecodedLockFile struct {
	SchemaRevision int                                `json:"schema_revision"`
	Skills         map[string]*previousSkillSelection `json:"skills"`
}

type lockFile struct {
	SchemaRevision int                       `json:"schema_revision"`
	Skills         map[string]skillSelection `json:"skills"`
}

type lock struct {
	SchemaRevision int
	Skills         map[string]bool
	Expressions    map[string]string
	Remote         map[string]remoteSkillRef
}

func (l *lock) enabled(name string) (enabledValue, bool) {
	if value, ok := l.Skills[name]; ok {
		return enabledValue{Boolean: new(value)}, true
	}
	if expression, ok := l.Expressions[name]; ok {
		return enabledValue{Expression: expression}, true
	}
	return enabledValue{}, false
}

func (l *lock) setEnabled(name string, value enabledValue) {
	if value.Boolean != nil {
		l.Skills[name] = *value.Boolean
		delete(l.Expressions, name)
		return
	}
	l.Expressions[name] = value.Expression
	delete(l.Skills, name)
}

func (l *lock) deleteEnabled(name string) {
	delete(l.Skills, name)
	delete(l.Expressions, name)
}

func loadLock(project string) (lock, error) {
	path := filepath.Join(project, lockName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newLock(), nil
	}
	if err != nil {
		return lock{}, fmt.Errorf("read %s: %w", path, err)
	}
	var revision struct {
		SchemaRevision int `json:"schema_revision"`
	}
	if err := json.Unmarshal(data, &revision); err != nil {
		return lock{}, fmt.Errorf("decode %s: %w", path, err)
	}

	result := newLock()
	switch revision.SchemaRevision {
	case legacyLockSchemaRevision:
		var legacy struct {
			SchemaRevision int             `json:"schema_revision"`
			Skills         map[string]bool `json:"skills"`
		}
		if err := decodeLockJSON(data, &legacy); err != nil {
			return lock{}, fmt.Errorf("decode %s: %w", path, err)
		}
		if legacy.Skills == nil {
			return lock{}, fmt.Errorf("decode %s: missing skills", path)
		}
		result.SchemaRevision = legacyLockSchemaRevision
		result.Skills = legacy.Skills
	case previousLockSchemaRevision:
		var previous previousDecodedLockFile
		if err := decodeLockJSON(data, &previous); err != nil {
			return lock{}, fmt.Errorf("decode %s: %w", path, err)
		}
		if previous.Skills == nil {
			return lock{}, fmt.Errorf("decode %s: missing skills", path)
		}
		result.SchemaRevision = previousLockSchemaRevision
		for name, selection := range previous.Skills {
			if selection == nil ||
				(selection.Enabled == nil && selection.Remote == nil) {
				return lock{}, fmt.Errorf(
					"decode %s: skill %q has neither enabled state nor remote metadata",
					path,
					name,
				)
			}
			converted := &skillSelection{Remote: selection.Remote}
			if selection.Enabled != nil {
				converted.Enabled = &enabledValue{Boolean: new(*selection.Enabled)}
			}
			if err := loadSelection(path, name, converted, &result); err != nil {
				return lock{}, err
			}
		}
	case lockSchemaRevision:
		var current decodedLockFile
		if err := decodeLockJSON(data, &current); err != nil {
			return lock{}, fmt.Errorf("decode %s: %w", path, err)
		}
		if current.Skills == nil {
			return lock{}, fmt.Errorf("decode %s: missing skills", path)
		}
		result.SchemaRevision = lockSchemaRevision
		for name, selection := range current.Skills {
			if selection == nil ||
				(selection.Enabled == nil && selection.Remote == nil) {
				return lock{}, fmt.Errorf(
					"decode %s: skill %q has neither enabled state nor remote metadata",
					path,
					name,
				)
			}
			if err := loadSelection(path, name, selection, &result); err != nil {
				return lock{}, err
			}
		}
	default:
		return lock{}, fmt.Errorf(
			"decode %s: unsupported schema revision %d; binary supports %d",
			path,
			revision.SchemaRevision,
			lockSchemaRevision,
		)
	}
	return result, nil
}

func loadSelection(path, name string, selection *skillSelection, result *lock) error {
	if selection.Enabled != nil {
		result.setEnabled(name, *selection.Enabled)
	}
	if selection.Remote == nil {
		return nil
	}
	if selection.Remote.Name != name {
		return fmt.Errorf(
			"decode %s: skill %q remote name is %q",
			path,
			name,
			selection.Remote.Name,
		)
	}
	if err := selection.Remote.validate(); err != nil {
		return fmt.Errorf("decode %s: skill %q: %w", path, name, err)
	}
	result.Remote[name] = *selection.Remote
	return nil
}

func decodeLockJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected data after lock")
	}
	return nil
}

func saveLock(project string, value lock) error {
	path := filepath.Join(project, lockName)
	serialized := lockFile{
		SchemaRevision: lockSchemaRevision,
		Skills: make(
			map[string]skillSelection,
			len(value.Skills)+len(value.Expressions)+len(value.Remote),
		),
	}
	for name, enabled := range value.Skills {
		serialized.Skills[name] = skillSelection{
			Enabled: &enabledValue{Boolean: new(enabled)},
		}
	}
	for name, expression := range value.Expressions {
		if _, duplicate := value.Skills[name]; duplicate {
			return fmt.Errorf("write %s: skill %q has multiple enabled values", path, name)
		}
		serialized.Skills[name] = skillSelection{
			Enabled: &enabledValue{Expression: expression},
		}
	}
	for name, ref := range value.Remote {
		selection := serialized.Skills[name]
		selection.Remote = new(ref)
		serialized.Skills[name] = selection
	}
	temp, err := os.CreateTemp(project, "."+lockName+"-")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	name := temp.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temp)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(serialized)
	if err == nil {
		err = temp.Chmod(0o644)
	}
	err = errors.Join(err, temp.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func updateLock(project, coordinationDir string, update func(*lock) (bool, error)) error {
	projectInfo, err := os.Stat(project)
	if err != nil {
		return fmt.Errorf("inspect selection lock target %s: %w", project, err)
	}
	stat, ok := projectInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect selection lock target %s: filesystem identity is unavailable", project)
	}
	if err := os.MkdirAll(coordinationDir, 0o700); err != nil {
		return fmt.Errorf("create selection lock directory: %w", err)
	}
	lockPath := filepath.Join(
		coordinationDir,
		fmt.Sprintf("%x-%x.lock", stat.Dev, stat.Ino),
	)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", lockPath, err)
	}
	defer file.Close()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()

	value, err := loadLock(project)
	if err != nil {
		return err
	}
	changed, err := update(&value)
	if err != nil || !changed {
		return err
	}
	return saveLock(project, value)
}

func newLock() lock {
	return lock{
		SchemaRevision: lockSchemaRevision,
		Skills:         make(map[string]bool),
		Expressions:    make(map[string]string),
		Remote:         make(map[string]remoteSkillRef),
	}
}
