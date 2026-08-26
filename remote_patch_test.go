package main

import (
	"strings"
	"testing"
)

func TestRemoteSkillUnifiedPatchRoundTrip(t *testing.T) {
	for name, test := range map[string]struct {
		original string
		edited   string
	}{
		"replace line": {
			original: "before\n",
			edited:   "after\n",
		},
		"separate hunks": {
			original: "one\n2\n3\n4\n5\n6\n7\n8\n9\nten\n",
			edited:   "ONE\n2\n3\n4\n5\n6\n7\n8\n9\nTEN\n",
		},
		"create content": {
			edited: "created\n",
		},
		"remove content": {
			original: "removed\n",
		},
		"add final newline": {
			original: "text",
			edited:   "text\n",
		},
		"remove final newline": {
			original: "text\n",
			edited:   "text",
		},
		"replace unterminated line": {
			original: "before",
			edited:   "after",
		},
		"preserve CRLF": {
			original: "before\r\nnext\r\n",
			edited:   "after\r\nnext\r\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			patch, err := makeRemoteSkillPatch([]byte(test.original), []byte(test.edited))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(patch, "--- a/SKILL.md\n+++ b/SKILL.md\n@@ ") {
				t.Fatalf("patch is not a conventional unified diff:\n%s", patch)
			}
			patched, err := applyRemoteSkillPatch([]byte(test.original), patch)
			if err != nil {
				t.Fatalf("apply patch:\n%s\nerror: %v", patch, err)
			}
			if string(patched) != test.edited {
				t.Fatalf("patched content = %q, want %q", patched, test.edited)
			}
		})
	}
}

func TestRemoteSkillUnifiedPatchIsReadable(t *testing.T) {
	patch, err := makeRemoteSkillPatch(
		[]byte("Command: old\n"),
		[]byte("Command: `skills-mgr get`\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/SKILL.md\n" +
		"+++ b/SKILL.md\n" +
		"@@ -1 +1 @@\n" +
		"-Command: old\n" +
		"+Command: `skills-mgr get`\n"
	if patch != want {
		t.Fatalf("patch =\n%s\nwant:\n%s", patch, want)
	}
	if strings.Contains(patch, "%0A") {
		t.Fatalf("patch contains percent-encoded newlines:\n%s", patch)
	}
}

func TestApplyRemoteSkillUnifiedPatchRejectsDamage(t *testing.T) {
	for name, patch := range map[string]string{
		"missing file headers": "@@ -1 +1 @@\n-before\n+after\n",
		"wrong file":           "--- a/other\n+++ b/other\n@@ -1 +1 @@\n-before\n+after\n",
		"bad source line":      "--- a/SKILL.md\n+++ b/SKILL.md\n@@ -1 +1 @@\n-other\n+after\n",
		"bad line count":       "--- a/SKILL.md\n+++ b/SKILL.md\n@@ -1,2 +1 @@\n-before\n+after\n",
		"invalid hunk line":    "--- a/SKILL.md\n+++ b/SKILL.md\n@@ -1 +1 @@\n?before\n+after\n",
		"unterminated middle line": "--- a/SKILL.md\n+++ b/SKILL.md\n@@ -1 +1,2 @@\n-before\n" +
			"+one\n\\ No newline at end of file\n+two\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := applyRemoteSkillPatch([]byte("before\n"), patch); err == nil {
				t.Fatal("damaged patch was accepted")
			}
		})
	}
}
