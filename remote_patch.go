package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

const (
	remotePatchOldFile = "a/SKILL.md"
	remotePatchNewFile = "b/SKILL.md"
)

func makeRemoteSkillPatch(original, edited []byte) (string, error) {
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        unifiedDiffLines(original),
		B:        unifiedDiffLines(edited),
		FromFile: remotePatchOldFile,
		ToFile:   remotePatchNewFile,
		Context:  3,
	})
}

func applyRemoteSkillPatch(original []byte, patch string) ([]byte, error) {
	lines := strings.SplitAfter(patch, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 3 ||
		lines[0] != "--- "+remotePatchOldFile+"\n" ||
		lines[1] != "+++ "+remotePatchNewFile+"\n" {
		return nil, fmt.Errorf("missing SKILL.md file headers")
	}

	source := contentLines(original)
	result := make([]string, 0, len(source))
	sourcePosition := 0
	hunks := 0
	for lineIndex := 2; lineIndex < len(lines); {
		oldPosition, oldCount, newPosition, newCount, err := parseUnifiedHunk(lines[lineIndex])
		if err != nil {
			return nil, err
		}
		if oldPosition < sourcePosition || oldPosition > len(source) || newPosition != len(result)+oldPosition-sourcePosition {
			return nil, fmt.Errorf("hunk position is outside the source")
		}
		result = append(result, source[sourcePosition:oldPosition]...)
		sourcePosition = oldPosition
		lineIndex++

		oldLines, newLines := 0, 0
		for lineIndex < len(lines) && !strings.HasPrefix(lines[lineIndex], "@@ -") {
			line := lines[lineIndex]
			if len(line) < 2 || !strings.HasSuffix(line, "\n") {
				return nil, fmt.Errorf("invalid hunk line")
			}
			contents := line[1:]
			lineIndex++
			if lineIndex < len(lines) && lines[lineIndex] == "\\ No newline at end of file\n" {
				contents = strings.TrimSuffix(contents, "\n")
				lineIndex++
			}

			switch line[0] {
			case ' ':
				if sourcePosition >= len(source) || source[sourcePosition] != contents {
					return nil, fmt.Errorf("context does not match source")
				}
				result = append(result, contents)
				sourcePosition++
				oldLines++
				newLines++
			case '-':
				if sourcePosition >= len(source) || source[sourcePosition] != contents {
					return nil, fmt.Errorf("deletion does not match source")
				}
				sourcePosition++
				oldLines++
			case '+':
				result = append(result, contents)
				newLines++
			default:
				return nil, fmt.Errorf("invalid hunk line")
			}
		}
		if oldLines != oldCount || newLines != newCount {
			return nil, fmt.Errorf("hunk line count does not match header")
		}
		hunks++
	}
	if hunks == 0 {
		return nil, fmt.Errorf("patch has no hunks")
	}
	result = append(result, source[sourcePosition:]...)
	for _, line := range result[:max(0, len(result)-1)] {
		if !strings.HasSuffix(line, "\n") {
			return nil, fmt.Errorf("non-final line has no newline")
		}
	}
	return []byte(strings.Join(result, "")), nil
}

func unifiedDiffLines(contents []byte) []string {
	lines := contentLines(contents)
	// difflib expects each logical line to carry its terminator, so attach the
	// standard marker to preserve a real file whose last line has no newline.
	if len(lines) != 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n\\ No newline at end of file\n"
	}
	return lines
}

func contentLines(contents []byte) []string {
	if len(contents) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(contents), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseUnifiedHunk(line string) (oldPosition, oldCount, newPosition, newCount int, err error) {
	line, ok := strings.CutPrefix(line, "@@ -")
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("missing hunk header")
	}
	if !strings.HasSuffix(line, "\n") {
		return 0, 0, 0, 0, fmt.Errorf("malformed hunk header")
	}
	line = strings.TrimSuffix(line, "\n")
	// Find the closing @@ and accept optional section text after it
	closeIdx := strings.Index(line, " @@")
	if closeIdx == -1 {
		return 0, 0, 0, 0, fmt.Errorf("malformed hunk header")
	}
	line = line[:closeIdx]
	oldRange, newRange, ok := strings.Cut(line, " +")
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("malformed hunk ranges")
	}
	oldPosition, oldCount, err = parseUnifiedRange(oldRange)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	newPosition, newCount, err = parseUnifiedRange(newRange)
	return oldPosition, oldCount, newPosition, newCount, err
}

func parseUnifiedRange(value string) (int, int, error) {
	startText, countText, hasCount := strings.Cut(value, ",")
	start, err := strconv.Atoi(startText)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("invalid hunk range")
	}
	count := 1
	if hasCount {
		count, err = strconv.Atoi(countText)
		if err != nil || count < 0 {
			return 0, 0, fmt.Errorf("invalid hunk range")
		}
	}
	if count == 0 {
		return start, count, nil
	}
	if start == 0 {
		return 0, 0, fmt.Errorf("invalid hunk range")
	}
	return start - 1, count, nil
}
