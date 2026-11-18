package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	managedSkillSource = "managed"
	claudePluginSource = "claude-plugin"
	grokPluginSource   = "grok-plugin"
	grokBundledSource  = "grok-bundled"
)

type manager struct {
	tuiCore  string
	cacheDir string
	global   bool
}

type discoveredSkill struct {
	Name                   string
	Description            string
	Path                   string
	Root                   string
	EntryPath              string
	Source                 string
	Editable               bool
	RemoteKey              string
	DisableModelInvocation bool
	Plugin                 string
	Vendor                 string
	UserInvocable          bool
	CompatibilityStatus    string
	ExternalEnabled        bool
	ContentMissing         bool
}

type remoteSkillRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Locator  string `json:"locator"`
}

func (r remoteSkillRef) key() string {
	sum := sha256.Sum256([]byte(r.Provider + "\x00" + r.ID))
	return hex.EncodeToString(sum[:])
}

type remoteToggleResult struct {
	Enabled bool
	tuiCoreSnapshot
}

type skillLocationResult struct {
	Skill   string
	Managed bool
	tuiCoreSnapshot
}

type modelInvocationResult struct {
	Skill    string
	Disabled bool
	tuiCoreSnapshot
}

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

type tuiCoreRequest struct {
	Op       string          `json:"op"`
	Global   bool            `json:"global"`
	Name     string          `json:"name,omitempty"`
	Path     string          `json:"path,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Draft    string          `json:"draft,omitempty"`
	Digest   string          `json:"digest,omitempty"`
	Key      string          `json:"key,omitempty"`
	Remote   *remoteSkillRef `json:"reference,omitempty"`
	Provider string          `json:"provider,omitempty"`
	Query    *string         `json:"query,omitempty"`
}

func (m *manager) tuiSnapshot(project string) (tuiCoreSnapshot, error) {
	var snapshot tuiCoreSnapshot
	err := m.callTUICore(project, tuiCoreRequest{Op: "snapshot"}, &snapshot)
	return snapshot, err
}

func (m *manager) tuiToggle(project string, skill discoveredSkill) (bool, error) {
	var enabled bool
	err := m.callTUICore(project, tuiCoreRequest{
		Op: "toggle", Name: skill.Name, Path: skill.Path, Key: skill.RemoteKey,
	}, &enabled)
	return enabled, err
}

func (m *manager) tuiToggleRemote(ctx context.Context, project string, ref remoteSkillRef) (remoteToggleResult, error) {
	var reply remoteToggleResult
	err := m.callTUICoreContext(ctx, project, tuiCoreRequest{Op: "toggle_remote", Remote: &ref}, &reply)
	return reply, err
}

func (m *manager) tuiToggleGrok(ctx context.Context, project, name string) (bool, error) {
	var enabled bool
	err := m.callTUICoreContext(ctx, project, tuiCoreRequest{Op: "toggle_grok", Name: name}, &enabled)
	return enabled, err
}

func (m *manager) tuiUninstall(ctx context.Context, project, name, key string) (tuiCoreSnapshot, error) {
	var reply tuiCoreSnapshot
	err := m.callTUICoreContext(ctx, project, tuiCoreRequest{Op: "uninstall", Name: name, Key: key}, &reply)
	return reply, err
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

func (m *manager) completeSkillEdit(project, skill, path, draft string, digest [sha256.Size]byte) (tuiCoreSnapshot, error) {
	var snapshot tuiCoreSnapshot
	err := m.callTUICore(project, tuiCoreRequest{
		Op: "edit", Name: skill, Path: path, Draft: draft, Digest: fmt.Sprintf("%x", digest),
	}, &snapshot)
	return snapshot, err
}

func (m *manager) applyEnabledDraft(project string, skill discoveredSkill, draft string) (tuiCoreSelection, error) {
	data, err := os.ReadFile(draft)
	if err != nil {
		return tuiCoreSelection{}, fmt.Errorf("read enabled editor draft: %w", err)
	}
	var selection tuiCoreSelection
	request := tuiCoreRequest{Op: "set_enabled", Name: skill.Name, Path: skill.Path, Key: skill.RemoteKey}
	switch value := strings.TrimSpace(string(data)); {
	case value == "":
	case value == "true" || value == "false" || strings.HasPrefix(value, "\""):
		if !json.Valid([]byte(value)) {
			return tuiCoreSelection{}, fmt.Errorf("decode enabled editor draft: invalid JSON value")
		}
		request.Value = json.RawMessage(value)
	default:
		request.Value, err = json.Marshal(value)
		if err != nil {
			return tuiCoreSelection{}, err
		}
	}
	err = m.callTUICore(project, request, &selection)
	return selection, err
}

func (m *manager) toggleModelInvocation(ctx context.Context, project string, expected discoveredSkill) (modelInvocationResult, error) {
	var result modelInvocationResult
	err := m.callTUICoreContext(ctx, project, tuiCoreRequest{
		Op: "toggle_invocation", Name: expected.Name, Path: expected.Path,
	}, &result)
	return result, err
}

func (m *manager) relocateSkill(project string, skill discoveredSkill) (skillLocationResult, error) {
	var result skillLocationResult
	err := m.callTUICore(project, tuiCoreRequest{
		Op: "relocate", Name: skill.Name, Path: skill.Path,
	}, &result)
	return result, err
}
