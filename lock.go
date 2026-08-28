package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

func (s skillSelection) clone() skillSelection {
	cloned := skillSelection{}
	if s.Enabled != nil {
		cloned.Enabled = new(*s.Enabled)
		if s.Enabled.Boolean != nil {
			cloned.Enabled.Boolean = new(*s.Enabled.Boolean)
		}
	}
	if s.Remote != nil {
		cloned.Remote = new(*s.Remote)
	}
	return cloned
}

// equal owns the rule for when two selections express the same enabled state,
// so callers deciding whether a write is needed cannot drift from it as
// enabledValue grows representations.
func (v enabledValue) equal(other enabledValue) bool {
	if v.Expression != other.Expression ||
		(v.Boolean == nil) != (other.Boolean == nil) {
		return false
	}
	return v.Boolean == nil || *v.Boolean == *other.Boolean
}

// wantsPlaceholder reports whether a configured selection should leave a
// harness placeholder behind. A conditional expression counts as wanting one:
// the stub is invisible to the model, so it cannot bypass a false condition;
// list, get, and run all evaluate the expression before answering.
func (v enabledValue) wantsPlaceholder() bool {
	return v.Boolean == nil || *v.Boolean
}

func (s skillSelection) equal(other skillSelection) bool {
	switch {
	case (s.Enabled == nil) != (other.Enabled == nil),
		(s.Remote == nil) != (other.Remote == nil):
		return false
	case s.Enabled != nil && !s.Enabled.equal(*other.Enabled):
		return false
	case s.Remote != nil && *s.Remote != *other.Remote:
		return false
	default:
		return true
	}
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
	Skills         map[string]skillSelection
}

func (l *lock) enabled(name string) (enabledValue, bool) {
	selection, ok := l.Skills[name]
	if !ok || selection.Enabled == nil {
		return enabledValue{}, false
	}
	return *selection.Enabled, true
}

func (l *lock) setEnabled(name string, value enabledValue) {
	selection := l.Skills[name]
	selection.Enabled = new(value)
	if value.Boolean != nil {
		selection.Enabled.Boolean = new(*value.Boolean)
	}
	l.Skills[name] = selection
}

func (l *lock) deleteEnabled(name string) {
	selection, ok := l.Skills[name]
	if !ok {
		return
	}
	selection.Enabled = nil
	if selection.Remote == nil {
		delete(l.Skills, name)
		return
	}
	l.Skills[name] = selection
}

func (l *lock) remote(name string) (remoteSkillRef, bool) {
	selection, ok := l.Skills[name]
	if !ok || selection.Remote == nil {
		return remoteSkillRef{}, false
	}
	return *selection.Remote, true
}

func (l *lock) setRemote(name string, ref remoteSkillRef) {
	selection := l.Skills[name]
	selection.Remote = new(ref)
	l.Skills[name] = selection
}

func (l *lock) deleteRemote(name string) {
	selection, ok := l.Skills[name]
	if !ok {
		return
	}
	selection.Remote = nil
	if selection.Enabled == nil {
		delete(l.Skills, name)
		return
	}
	l.Skills[name] = selection
}

func (l lock) clone() lock {
	cloned := lock{
		SchemaRevision: l.SchemaRevision,
		Skills:         make(map[string]skillSelection, len(l.Skills)),
	}
	for name, selection := range l.Skills {
		cloned.Skills[name] = selection.clone()
	}
	return cloned
}

func (l lock) equal(other lock) bool {
	return maps.EqualFunc(l.Skills, other.Skills, skillSelection.equal)
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
		if err := decodeStrictJSON(data, &legacy); err != nil {
			return lock{}, fmt.Errorf("decode %s: %w", path, err)
		}
		if legacy.Skills == nil {
			return lock{}, fmt.Errorf("decode %s: missing skills", path)
		}
		result.SchemaRevision = legacyLockSchemaRevision
		for name, enabled := range legacy.Skills {
			result.setEnabled(name, enabledValue{Boolean: new(enabled)})
		}
	case previousLockSchemaRevision:
		var previous previousDecodedLockFile
		if err := decodeStrictJSON(data, &previous); err != nil {
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
		if err := decodeStrictJSON(data, &current); err != nil {
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
	if selection.Remote != nil {
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
	}
	result.Skills[name] = *selection
	return nil
}

func saveLock(project string, value lock) error {
	path := filepath.Join(project, lockName)
	serialized := lockFile{
		SchemaRevision: lockSchemaRevision,
		Skills:         make(map[string]skillSelection, len(value.Skills)),
	}
	for name, selection := range value.Skills {
		if selection.Enabled == nil && selection.Remote == nil {
			return fmt.Errorf(
				"write %s: skill %q has neither enabled state nor remote metadata",
				path,
				name,
			)
		}
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
	if err := flockExclusive(file, lockPath); err != nil {
		return err
	}
	defer unlockFlock(file)

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
		Skills:         make(map[string]skillSelection),
	}
}

// flockExclusive waits in the kernel until the advisory lock is held, retrying
// the EINTR that signal delivery causes. Callers that must stay cancellable use
// flockExclusiveContext instead.
func flockExclusive(file *os.File, label string) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("lock %s: %w", label, err)
		}
		return nil
	}
}

// flockExclusiveContext polls for the advisory lock so a caller carrying a
// context stays cancellable while another process holds it, and reports the
// context's own error on cancellation so callers can distinguish it.
func flockExclusiveContext(ctx context.Context, file *os.File, label string) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		default:
			return fmt.Errorf("lock %s: %w", label, err)
		}
	}
}

func unlockFlock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
