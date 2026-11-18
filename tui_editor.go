package main

import (
	"crypto/sha256"
	"encoding/json"
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
	temporary, err := os.CreateTemp("", "skills-mgr-skill-*.md")
	if err != nil {
		return skillEdit{}, fmt.Errorf("create skill edit: %w", err)
	}
	draft := temporary.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(draft)
		}
	}()
	if err := temporary.Close(); err != nil {
		return skillEdit{}, err
	}
	var prepared struct {
		Path   string
		Digest [sha256.Size]byte
	}
	if err := m.manager.callTUICore(m.project, tuiCoreRequest{Op: "prepare_edit", Name: skill, Draft: draft}, &prepared); err != nil {
		return skillEdit{}, err
	}
	command, err := configuredEditor(draft)
	return skillEdit{command: command, draft: draft, layeredDigest: prepared.Digest,
		source: discoveredSkill{Name: skill, Path: prepared.Path}}, err
}

func (m model) enabledEditor(skill string) (_ *exec.Cmd, draft string, retErr error) {

	var value json.RawMessage
	if err := m.manager.callTUICore(m.project, tuiCoreRequest{Op: "enabled_value", Name: skill}, &value); err != nil {
		return nil, "", err
	}
	var contents []byte
	switch string(value) {
	case "null":
	case "true", "false":
		contents = value
	default:
		var expression string
		if err := json.Unmarshal(value, &expression); err != nil {
			return nil, "", fmt.Errorf("decode enabled value: %w", err)
		}
		contents = []byte(expression)
	}
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

	if len(contents) != 0 {
		if _, err := temp.Write(append(contents, '\n')); err != nil {
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
