package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-shellwords"
)

type skillEdit struct {
	command *exec.Cmd
	draft   string
	path    string
	digest  [sha256.Size]byte
}

func (m *model) editSkill(skill string) tea.Cmd {
	edit, err := m.editor(skill)
	if err != nil {
		m.status = "error: " + err.Error()
		return nil
	}
	m.busy = true
	m.status = "editing " + skill
	return tea.ExecProcess(edit.command, func(err error) tea.Msg {
		defer os.Remove(edit.draft)
		done := editDone{skill: skill, path: edit.path, editorErr: err}
		if err == nil {
			done.snapshot, done.refreshErr = m.manager.completeSkillEdit(
				m.project, skill, edit.path, edit.draft, edit.digest,
			)
		}
		return done
	})
}

func (m *model) editEnabledValue(skill discoveredSkill) tea.Cmd {
	command, draft, err := m.enabledEditor(skill.Name)
	if err != nil {
		m.status = "error: " + err.Error()
		return nil
	}
	m.busy = true
	m.status = "editing enabled value for " + skill.Name
	return tea.ExecProcess(command, func(err error) tea.Msg {
		defer os.Remove(draft)
		done := enabledEditDone{skill: skill.Name, editorErr: err}
		if err == nil {
			done.selection, done.refreshErr = m.manager.applyEnabledDraft(m.project, skill, draft)
		}
		return done
	})
}

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
	return skillEdit{command: command, draft: draft, path: prepared.Path, digest: prepared.Digest}, err
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
