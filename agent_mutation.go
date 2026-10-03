package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func (m *manager) setContext(ctx context.Context, project, name, value string, out io.Writer) error {
	skill, err := m.findSkill(project, name)
	if err != nil {
		return err
	}
	var desired enabledValue
	switch value {
	case "inherit":
	case "true", "false":
		desired.Boolean = new(value == "true")
	default:
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("selection condition is empty; use inherit to remove the override")
		}
		if _, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(value), "enabled"); err != nil {
			return fmt.Errorf("invalid selection condition: %w", err)
		}
		desired.Expression = value
	}
	if _, err := m.applyEnabledValue(ctx, project, skill, desired, value != "inherit"); err != nil {
		return err
	}
	layer := "project"
	if m.global {
		layer = "global"
	}
	_, err = fmt.Fprintf(out, "Updated %s (%s selection)\n", name, layer)
	return err
}

func (m *manager) editContext(ctx context.Context, project, name, expected string, input io.Reader, out io.Writer) error {
	skill, err := m.findSkill(project, name)
	if err != nil {
		return err
	}
	if !skill.Editable {
		return fmt.Errorf("skill %q is not editable at its discovered source", name)
	}
	// Serialize local manifest writes together with selection and placeholder changes.
	// Remote edits instead use the remote store's existing conflict-checked transaction.
	if skill.RemoteKey == "" {
		guard, err := m.lockManagedMutation(ctx)
		if err != nil {
			return err
		}
		defer closeExclusiveLock(guard)
	}
	original, err := m.skillManifest(skill)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(original)
	if expected != "" && expected != fmt.Sprintf("%x", digest) {
		return fmt.Errorf("skill %q changed: expected SHA256 %s, found %x", name, expected, digest)
	}
	contents, err := io.ReadAll(input)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil {
		return err
	}
	frontmatter, body, status, err := readFrontmatter(bytes.NewReader(contents))
	if err != nil {
		return err
	}
	metadata, valid := skillFromFrontmatter(frontmatter)
	if status != frontmatterValid || !valid {
		return fmt.Errorf("edited SKILL.md needs valid name and description frontmatter")
	}
	bodyContents, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(bodyContents)) == 0 {
		return fmt.Errorf("missing body or frontmatter-only placeholder")
	}
	if skill.RemoteKey != "" {
		oldFrontmatter, _, _, err := readFrontmatter(bytes.NewReader(original))
		if err != nil {
			return err
		}
		oldMetadata, _ := skillFromFrontmatter(oldFrontmatter)
		if metadata.Name != skill.Name || metadata.DisableModelInvocation != oldMetadata.DisableModelInvocation {
			return fmt.Errorf("remote identity and invocation metadata are not changed by content edits")
		}
		ref, err := m.persistedRemoteRef(skill.RemoteKey, skill.Name)
		if err != nil {
			return err
		}
		if err := m.remoteStore.savePatch(ctx, ref, skill.Path, digest, contents); err != nil {
			return err
		}
	} else {
		if metadata.Name != name {
			owners, err := m.discoverSkillsNamed(project, "", metadata.Name)
			if err != nil {
				return err
			}
			if len(owners) != 0 {
				return fmt.Errorf("skill name %q is already owned by %s", metadata.Name, owners[0].Path)
			}
		}
		info, err := os.Stat(skill.Path)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(skill.Path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, original) {
			return fmt.Errorf("skill %q changed during edit", name)
		}
		if err := writeSkillManifest(skill.Path, contents, info.Mode().Perm()); err != nil {
			return err
		}
		if _, _, err := m.refreshEditedSkillLocked(project, name, skill.Path); err != nil {
			current, readErr := os.ReadFile(skill.Path)
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			if !bytes.Equal(current, contents) {
				return errors.Join(err, fmt.Errorf("source changed during edit rollback"))
			}
			return errors.Join(err, writeSkillManifest(skill.Path, original, info.Mode().Perm()))
		}
	}
	_, err = fmt.Fprintf(out, "Updated %s\n", metadata.Name)
	return err
}

func writeSkillManifest(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".skills-mgr-edit-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = file.Chmod(mode)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (m *manager) agentCommand(ctx context.Context, project string, args []string, input io.Reader, out, diagnostics io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command, rest := args[0], args[1:]
	if command == "set" || command == "edit" {
		if len(rest) > 0 && rest[0] == "-g" {
			m.global = true
			rest = rest[1:]
		}
		if err := m.useGlobalAtHome(project); err != nil {
			return err
		}
	}
	switch command {
	case "inspect":
		if len(rest) != 1 {
			return fmt.Errorf("usage: skills-mgr inspect <skill>")
		}
		return m.inspectContext(ctx, project, rest[0], out)
	case "check":
		if len(rest) == 0 {
			return fmt.Errorf("usage: skills-mgr check <target> [target...]")
		}
		return m.checkContext(ctx, project, rest, out, diagnostics)
	case "set":
		if len(rest) != 2 {
			return fmt.Errorf("usage: skills-mgr set [-g] <skill> <true|false|condition|inherit>")
		}
		return m.setContext(ctx, project, rest[0], rest[1], out)
	case "edit":
		if len(rest) != 3 && len(rest) != 5 || len(rest) >= 2 && rest[1] != "--file" || len(rest) == 5 && rest[3] != "--expect-sha256" {
			return fmt.Errorf("usage: skills-mgr edit [-g] <skill> --file <path|-> [--expect-sha256 <digest>]")
		}
		expected := ""
		if len(rest) == 5 {
			expected = rest[4]
			if decoded, err := hex.DecodeString(expected); err != nil || len(decoded) != sha256.Size {
				return fmt.Errorf("expected SHA256 must have 64 hexadecimal digits")
			}
			expected = strings.ToLower(expected)
		}
		if rest[2] != "-" {
			file, err := os.Open(rest[2])
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("edit input is not a regular file")
			}
			input = file
		}
		if closer, ok := input.(io.ReadCloser); ok {
			stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
			defer stop()
		}
		return m.editContext(ctx, project, rest[0], expected, input, out)
	default:
		return fmt.Errorf("unknown agent command %q", command)
	}
}
