package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

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
	if !filepath.IsAbs(m.tuiCore) {
		return nil, selectionState{}, errors.New("skills-mgr core path is not absolute")
	}
	request, err := json.Marshal(struct {
		Op     string `json:"op"`
		Global bool   `json:"global"`
	}{Op: "snapshot", Global: m.global})
	if err != nil {
		return nil, selectionState{}, err
	}
	command := exec.Command(m.tuiCore, "__tui")
	command.Dir = project
	command.Stdin = bytes.NewReader(request)
	output, err := command.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) != 0 {
			return nil, selectionState{}, fmt.Errorf("read catalog from Rust: %s", bytes.TrimSpace(exit.Stderr))
		}
		return nil, selectionState{}, fmt.Errorf("read catalog from Rust: %w", err)
	}
	var snapshot struct {
		Skills             []discoveredSkill
		Selected           map[string]bool
		GlobalSelected     map[string]bool
		ProjectSelected    map[string]bool
		GlobalExpressions  map[string]string
		ProjectExpressions map[string]string
	}
	if err := json.Unmarshal(output, &snapshot); err != nil {
		return nil, selectionState{}, fmt.Errorf("decode Rust catalog: %w", err)
	}
	return snapshot.Skills, selectionState{
		selected:           snapshot.Selected,
		globalSelected:     snapshot.GlobalSelected,
		projectSelected:    snapshot.ProjectSelected,
		globalExpressions:  snapshot.GlobalExpressions,
		projectExpressions: snapshot.ProjectExpressions,
	}, nil
}
