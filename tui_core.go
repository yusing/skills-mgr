package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
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
	Op     string          `json:"op"`
	Global bool            `json:"global"`
	Name   string          `json:"name,omitempty"`
	Path   string          `json:"path,omitempty"`
	Value  *enabledValue   `json:"value,omitempty"`
	Draft  string          `json:"draft,omitempty"`
	Digest string          `json:"digest,omitempty"`
	Key    string          `json:"key,omitempty"`
	Remote *remoteSkillRef `json:"reference,omitempty"`
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
	if m.tuiCore == "" {
		return m.toggle(project, skill.Name, skill.RemoteKey)
	}
	var enabled bool
	err := m.callTUICore(project, tuiCoreRequest{
		Op: "toggle", Name: skill.Name, Path: skill.Path, Key: skill.RemoteKey,
	}, &enabled)
	return enabled, err
}

func (m *manager) tuiToggleRemote(ctx context.Context, project string, ref remoteSkillRef) (remoteToggleResult, error) {
	if m.tuiCore == "" {
		return m.toggleRemote(ctx, project, ref)
	}
	var reply struct {
		Enabled bool
		tuiCoreSnapshot
	}
	err := m.callTUICoreContext(ctx, project, tuiCoreRequest{Op: "toggle_remote", Remote: &ref}, &reply)
	return newRemoteToggleResult(ref.Name, reply.Enabled, reply.Skills, reply.Selected), err
}

func (m *manager) tuiUninstall(ctx context.Context, project, name, key string) (remoteUninstallResult, error) {
	if m.tuiCore == "" {
		return m.uninstallRemote(ctx, project, name, key)
	}
	var reply tuiCoreSnapshot
	err := m.callTUICoreContext(ctx, project, tuiCoreRequest{Op: "uninstall", Name: name, Key: key}, &reply)
	return remoteUninstallResult{
		Skill: name, Skills: reply.Skills, Selected: reply.Selected,
		GlobalSelected: reply.GlobalSelected, ProjectSelected: reply.ProjectSelected,
	}, err
}

func (m *manager) callTUICore(project string, input tuiCoreRequest, response any) error {
	return m.callTUICoreContext(context.Background(), project, input, response)
}

func (m *manager) callTUICoreContext(ctx context.Context, project string, input tuiCoreRequest, response any) error {
	if !filepath.IsAbs(m.tuiCore) {
		return errors.New("skills-mgr core path is not absolute")
	}
	input.Global = m.global
	request, err := json.Marshal(input)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, m.tuiCore, "__tui")
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.Dir = project
	command.Stdin = bytes.NewReader(request)
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
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
