package main

import (
	"crypto/sha256"

	"fmt"
	"os"
	"os/exec"

	"github.com/mattn/go-shellwords"
)

func configuredEditor(file string) (*exec.Cmd, error) {
	parts, err := shellwords.Parse(os.Getenv("EDITOR"))
	if err != nil || len(parts) == 0 {
		return nil, fmt.Errorf("$EDITOR is not set or invalid")
	}
	return exec.Command(parts[0], append(parts[1:], file)...), nil
}

func (m model) editor(skill string) (_ skillEdit, retErr error) {
	discovered, err := m.manager.findSkill(m.project, skill)
	if err != nil {
		return skillEdit{}, err
	}
	if !discovered.Editable {
		return skillEdit{}, fmt.Errorf("skill %q is not editable at its discovered source", skill)
	}
	file := discovered.Path
	if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
		return skillEdit{}, fmt.Errorf("missing %s", file)
	}
	edit := skillEdit{source: discovered}
	if discovered.RemoteKey != "" {
		original, err := os.ReadFile(file)
		if err != nil {
			return skillEdit{}, fmt.Errorf("read remote skill: %w", err)
		}
		ref, err := m.manager.persistedRemoteRef(discovered.RemoteKey, discovered.Name)
		if err != nil {
			return skillEdit{}, err
		}
		contents, err := m.manager.remoteStore.layeredContent(ref, original)
		if err != nil {
			return skillEdit{}, err
		}
		temporary, err := os.CreateTemp("", "skills-mgr-remote-skill-*.md")
		if err != nil {
			return skillEdit{}, fmt.Errorf("create remote skill edit: %w", err)
		}
		edit.draft = temporary.Name()
		edit.layeredDigest = sha256.Sum256(contents)
		defer func() {
			if retErr != nil {
				_ = temporary.Close()
				_ = os.Remove(edit.draft)
			}
		}()
		if err := temporary.Chmod(0o600); err != nil {
			return skillEdit{}, err
		}
		if _, err := temporary.Write(contents); err != nil {
			return skillEdit{}, fmt.Errorf("write remote skill edit: %w", err)
		}
		if err := temporary.Close(); err != nil {
			return skillEdit{}, fmt.Errorf("close remote skill edit: %w", err)
		}
		file = edit.draft
	}
	edit.command, err = configuredEditor(file)
	return edit, err
}

func (m model) enabledEditor(skill string) (_ *exec.Cmd, draft string, retErr error) {
	value, err := loadLock(m.manager.lockDir(m.project))
	if err != nil {
		return nil, "", err
	}
	current, configured := value.enabled(skill)
	temp, err := os.CreateTemp("", "skills-mgr-enabled-*.json")
	if err != nil {
		return nil, "", fmt.Errorf("create enabled editor draft: %w", err)
	}
	draft = temp.Name()
	defer func() {
		if retErr != nil {
			_ = temp.Close()
			_ = os.Remove(temp.Name())
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return nil, "", fmt.Errorf("secure enabled editor draft: %w", err)
	}
	if configured {
		var data []byte
		if current.Boolean != nil {
			data = fmt.Appendf(data, "%t", *current.Boolean)
		} else {
			data = append(data, current.Expression...)
		}
		data = append(data, '\n')
		if _, err := temp.Write(data); err != nil {
			return nil, "", fmt.Errorf("write enabled editor draft: %w", err)
		}
	}
	if err := temp.Close(); err != nil {
		return nil, "", fmt.Errorf("close enabled editor draft: %w", err)
	}
	command, err := configuredEditor(draft)
	if err != nil {
		return nil, "", err
	}
	return command, draft, nil
}
