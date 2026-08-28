package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"

	"errors"
	"fmt"
	"io"

	"os"

	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"unicode"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	yamlToken "github.com/goccy/go-yaml/token"
)

// skillManifestName is the filename every harness reads a skill's frontmatter
// from, so it is fixed by those harnesses rather than chosen here.
const skillManifestName = "SKILL.md"

type skillFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

type frontmatterStatus uint8

const (
	frontmatterAbsent frontmatterStatus = iota
	frontmatterValid
	frontmatterMalformed
)

func parseSkill(path string) (discoveredSkill, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return discoveredSkill{}, false, nil
		}
		return discoveredSkill{}, false, err
	}
	defer file.Close()
	frontmatter, _, status, err := readFrontmatter(file)
	if err != nil {
		return discoveredSkill{}, false, err
	}
	if status != frontmatterValid {
		return discoveredSkill{}, false, nil
	}
	skill, ok := skillFromFrontmatter(frontmatter)
	return skill, ok, nil
}

// skillFromFrontmatter converts frontmatter text into the skill it describes,
// reporting false when the metadata does not name a usable skill. Callers that
// already hold the frontmatter text use this instead of reopening the file.
func skillFromFrontmatter(frontmatter string) (discoveredSkill, bool) {
	var metadata skillFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		return discoveredSkill{}, false
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.Join(strings.Fields(metadata.Description), " ")
	if !validSkillName(metadata.Name) || !validSkillDescription(metadata.Description) {
		return discoveredSkill{}, false
	}
	return discoveredSkill{
		Name:                   metadata.Name,
		Description:            metadata.Description,
		DisableModelInvocation: metadata.DisableModelInvocation,
	}, true
}

func toggleModelInvocationFrontmatter(data []byte) ([]byte, bool, error) {
	frontmatter, body, status, err := readFrontmatter(bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	if status != frontmatterValid {
		return nil, false, fmt.Errorf("SKILL.md has invalid frontmatter")
	}
	bodyLength, err := io.Copy(io.Discard, body)
	if err != nil {
		return nil, false, err
	}
	metadataStart := bytes.IndexByte(data, '\n') + 1
	metadataEnd := metadataStart + len(frontmatter)
	if metadataStart == 0 || metadataEnd > len(data)-int(bodyLength) {
		return nil, false, fmt.Errorf("SKILL.md frontmatter boundaries are invalid")
	}

	frontmatterBytes := []byte(frontmatter)
	file, err := parser.ParseBytes(frontmatterBytes, parser.ParseComments)
	if err != nil {
		return nil, false, fmt.Errorf("parse SKILL.md frontmatter: %w", err)
	}
	if len(file.Docs) != 1 {
		return nil, false, fmt.Errorf("SKILL.md frontmatter must contain one document")
	}
	mapping, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, false, fmt.Errorf("SKILL.md frontmatter must be a mapping")
	}
	var metadata skillFrontmatter
	if err := yaml.Unmarshal(frontmatterBytes, &metadata); err != nil {
		return nil, false, fmt.Errorf("decode SKILL.md frontmatter: %w", err)
	}
	disabled := !metadata.DisableModelInvocation
	tokenOffset := func(current *yamlToken.Token, value string) (int, error) {
		start := strings.Index(current.Origin, value)
		if start < 0 {
			return 0, fmt.Errorf("locate YAML token")
		}
		for previous := current.Prev; previous != nil; previous = previous.Prev {
			start += len(previous.Origin)
		}
		return start, nil
	}
	preserveBooleanCase := func(original, replacement string) string {
		switch {
		case original == strings.ToUpper(original):
			return strings.ToUpper(replacement)
		case original == strings.ToLower(original):
			return replacement
		case len(original) > 1 &&
			original[:1] == strings.ToUpper(original[:1]) &&
			original[1:] == strings.ToLower(original[1:]):
			return strings.ToUpper(replacement[:1]) + replacement[1:]
		default:
			return replacement
		}
	}
	taggedBooleanReplacement := func(value string) (string, bool) {
		switch strings.ToLower(value) {
		case "yes", "no":
			replacement := "no"
			if disabled {
				replacement = "yes"
			}
			return preserveBooleanCase(value, replacement), true
		case "true", "false":
			return preserveBooleanCase(value, strconv.FormatBool(disabled)), true
		case "t", "f":
			replacement := "f"
			if disabled {
				replacement = "t"
			}
			return preserveBooleanCase(value, replacement), true
		case "1", "0":
			if disabled {
				return "1", true
			}
			return "0", true
		default:
			return "", false
		}
	}
	type edit struct {
		start       int
		end         int
		replacement string
	}
	var edits []edit
	for _, entry := range mapping.Values {
		var key string
		if err := yaml.NodeToValue(entry.Key, &key); err != nil {
			return nil, false, fmt.Errorf("decode SKILL.md frontmatter key: %w", err)
		}
		if key != "disable-model-invocation" {
			continue
		}
		node := entry.Value
		taggedBoolean := false
		var scalar *yamlToken.Token
		for scalar == nil {
			switch current := node.(type) {
			case *ast.AnchorNode:
				node = current.Value
			case *ast.TagNode:
				taggedBoolean = taggedBoolean || current.GetToken().Value == "!!bool"
				node = current.Value
			case *ast.BoolNode:
				scalar = current.GetToken()
			case *ast.StringNode, *ast.IntegerNode:
				if !taggedBoolean {
					return nil, false, fmt.Errorf(
						"disable-model-invocation must be a boolean value, not %T",
						node,
					)
				}
				scalar = current.GetToken()
			default:
				return nil, false, fmt.Errorf(
					"disable-model-invocation must be a boolean value, not %T",
					node,
				)
			}
		}
		replacement := strconv.FormatBool(disabled)
		if taggedBoolean {
			var ok bool
			replacement, ok = taggedBooleanReplacement(scalar.Value)
			if !ok {
				return nil, false, fmt.Errorf(
					"disable-model-invocation has unsupported !!bool value %q",
					scalar.Value,
				)
			}
		}
		start, err := tokenOffset(scalar, scalar.Value)
		if err != nil {
			return nil, false, fmt.Errorf("locate disable-model-invocation token: %w", err)
		}
		end := start + len(scalar.Value)
		if end > len(frontmatterBytes) || string(frontmatterBytes[start:end]) != scalar.Value {
			return nil, false, fmt.Errorf("locate disable-model-invocation value")
		}
		edits = append(edits, edit{start: start, end: end, replacement: replacement})
	}
	lineEnding := "\n"
	if bytes.HasSuffix(data[:metadataStart], []byte("\r\n")) {
		lineEnding = "\r\n"
	}
	rendered := slices.Clone(frontmatterBytes)
	if len(edits) == 0 {
		if mapping.IsFlowStyle {
			if mapping.End == nil {
				return nil, false, fmt.Errorf("locate flow-style frontmatter closing brace")
			}
			closing, err := tokenOffset(mapping.End, mapping.End.Value)
			if err != nil || closing >= len(rendered) || rendered[closing] != '}' {
				return nil, false, fmt.Errorf("locate flow-style frontmatter closing brace")
			}
			addition := fmt.Appendf(
				nil,
				", disable-model-invocation: %t",
				disabled,
			)
			rendered = slices.Insert(rendered, closing, addition...)
		} else {
			if len(rendered) > 0 && !bytes.HasSuffix(rendered, []byte(lineEnding)) {
				rendered = append(rendered, lineEnding...)
			}
			rendered = fmt.Appendf(
				rendered,
				"disable-model-invocation: %t%s",
				disabled,
				lineEnding,
			)
		}
	} else {
		slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
		for _, edit := range slices.Backward(edits) {
			rendered = slices.Replace(
				rendered,
				edit.start,
				edit.end,
				[]byte(edit.replacement)...,
			)
		}
	}
	updated := make([]byte, 0, len(data)+len(rendered)-len(frontmatterBytes))
	updated = append(updated, data[:metadataStart]...)
	updated = append(updated, rendered...)
	updated = append(updated, data[metadataEnd:]...)
	return updated, disabled, nil
}

func toggleModelInvocationFile(
	ctx context.Context,
	coordinationDir string,
	path string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(coordinationDir, 0o700); err != nil {
		return false, fmt.Errorf("create model invocation lock directory: %w", err)
	}
	lockKey := sha256.Sum256([]byte(path))
	lockPath := filepath.Join(
		coordinationDir,
		fmt.Sprintf("skill-model-invocation-%x.lock", lockKey),
	)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open model invocation lock: %w", err)
	}
	defer lock.Close()
	if err := flockExclusiveContext(ctx, lock, "model invocation update"); err != nil {
		return false, err
	}
	defer unlockFlock(lock)
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("skill source %s is not a regular file", path)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	updated, disabled, err := toggleModelInvocationFrontmatter(original)
	if err != nil {
		return false, err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".skill-model-invocation-")
	if err != nil {
		return false, fmt.Errorf("create temporary SKILL.md: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	err = temporary.Chmod(info.Mode().Perm())
	if err == nil {
		_, err = temporary.Write(updated)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err != nil {
		return false, fmt.Errorf("write temporary SKILL.md: %w", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("recheck %s: %w", path, err)
	}
	currentInfo, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("recheck %s: %w", path, err)
	}
	if !os.SameFile(info, currentInfo) || currentInfo.Mode() != info.Mode() ||
		!bytes.Equal(current, original) {
		return false, fmt.Errorf("skill source changed while model invocation was updating")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.Rename(name, path); err != nil {
		return false, fmt.Errorf("replace %s: %w", path, err)
	}
	return disabled, nil
}

func readFrontmatter(input io.Reader) (string, io.Reader, frontmatterStatus, error) {
	reader := bufio.NewReader(input)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", nil, frontmatterMalformed, err
	}
	if frontmatterLine(strings.TrimPrefix(line, "\uFEFF")) != "---" {
		return "", io.MultiReader(strings.NewReader(line), reader), frontmatterAbsent, nil
	}
	if errors.Is(err, io.EOF) {
		return "", nil, frontmatterMalformed, nil
	}
	size := len(line)
	var metadata strings.Builder
	for {
		line, err = reader.ReadString('\n')
		size += len(line)
		if size > maxFrontmatterBytes {
			return "", nil, frontmatterMalformed, nil
		}
		normalized := frontmatterLine(line)
		if normalized == "---" {
			return metadata.String(), reader, frontmatterValid, nil
		}
		if errors.Is(err, io.EOF) {
			return "", nil, frontmatterMalformed, nil
		}
		if err != nil {
			return "", nil, frontmatterMalformed, err
		}
		metadata.WriteString(line)
	}
}

func frontmatterLine(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

func validSkillName(name string) bool {
	if name == "" || len(name) > maxSkillNameLen {
		return false
	}
	for _, character := range name {
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '-' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func validSkillDescription(description string) bool {
	if description == "" {
		return false
	}
	for _, character := range description {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func pathDepth(relative string) int {
	if relative == "." || relative == "" {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(relative), "/"))
}
