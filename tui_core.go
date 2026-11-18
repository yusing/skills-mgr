package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

type tuiCoreSelection struct {
	Selected           map[string]bool
	GlobalSelected     map[string]bool
	ProjectSelected    map[string]bool
	GlobalExpressions  map[string]string
	ProjectExpressions map[string]string
}

type tuiCoreSnapshot struct {
	Skills []discoveredSkill
	tuiCoreSelection
}

func (s tuiCoreSelection) state() selectionState {
	return selectionState{
		selected:           s.Selected,
		globalSelected:     s.GlobalSelected,
		projectSelected:    s.ProjectSelected,
		globalExpressions:  s.GlobalExpressions,
		projectExpressions: s.ProjectExpressions,
	}
}

type tuiCoreRequest struct {
	Op     string        `json:"op"`
	Global bool          `json:"global"`
	Name   string        `json:"name,omitempty"`
	Path   string        `json:"path,omitempty"`
	Value  *enabledValue `json:"value,omitempty"`
	Draft  string        `json:"draft,omitempty"`
	Digest string        `json:"digest,omitempty"`
}

func (m *manager) tuiSnapshot(project string) ([]discoveredSkill, selectionState, error) {
	// The original Go command remains usable until release cutover. When the
	// Rust command starts the UI, Rust supplies this single startup read.
	if m.tuiCore == "" {
		discovered, err := m.managementSkills(project, "")
		if err != nil {
			return nil, selectionState{}, err
		}
		native, err := m.nativeSkills(project)
		if err != nil {
			return nil, selectionState{}, err
		}
		selection, err := m.selectionState(project, nil)
		return append(discovered, native...), selection, err
	}
	var snapshot tuiCoreSnapshot
	err := m.callTUICore(project, tuiCoreRequest{Op: "snapshot"}, &snapshot)
	return snapshot.Skills, snapshot.state(), err
}

func (m *manager) tuiToggle(project string, skill discoveredSkill) (bool, error) {
	if m.tuiCore == "" || skill.RemoteKey != "" {
		return m.toggle(project, skill.Name, skill.RemoteKey)
	}
	var enabled bool
	err := m.callTUICore(project, tuiCoreRequest{
		Op: "toggle", Name: skill.Name, Path: skill.Path,
	}, &enabled)
	return enabled, err
}

func (m *manager) callTUICore(project string, input tuiCoreRequest, response any) error {
	if !filepath.IsAbs(m.tuiCore) {
		return errors.New("skills-mgr core path is not absolute")
	}
	input.Global = m.global
	request, err := json.Marshal(input)
	if err != nil {
		return err
	}
	command := exec.Command(m.tuiCore, "__tui")
	command.Dir = project
	command.Stdin = bytes.NewReader(request)
	output, err := command.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) != 0 {
			return fmt.Errorf("Rust %s: %s", input.Op, bytes.TrimSpace(exit.Stderr))
		}
		return fmt.Errorf("Rust %s: %w", input.Op, err)
	}
	if err := json.Unmarshal(output, response); err != nil {
		return fmt.Errorf("decode Rust %s response: %w", input.Op, err)
	}
	return nil
}
