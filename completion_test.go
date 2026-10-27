package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCompleteSkillsUsesSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	for _, name := range []string{"alpha", "beta", "hidden"} {
		body := skillFile(name, "A skill.", "Body.")
		if name == "hidden" {
			body = strings.Replace(body, "description: A skill.", "description: A skill.\ndisable-model-invocation: true", 1)
		}
		writeFile(t, filepath.Join(manager.paths.userSkills, name, "SKILL.md"), body)
	}
	if err := saveLock(project, testLock(map[string]bool{"alpha": true, "beta": false, "hidden": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		enabled bool
		want    string
	}{{false, "alpha\nbeta\nhidden\n"}, {true, "alpha\nhidden\n"}} {
		var output bytes.Buffer
		if err := manager.completeSkills(t.Context(), project, tc.enabled, &output); err != nil {
			t.Fatal(err)
		}
		if output.String() != tc.want {
			t.Errorf("enabled=%t: got %q, want %q", tc.enabled, output.String(), tc.want)
		}
	}
}

func TestFishCompletions(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "skills-mgr"), "#!/bin/sh\ncase \"$2\" in all) printf 'alpha\\nbeta\\n';; enabled) printf 'alpha\\n';; esac\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	for _, tc := range []struct {
		command string
		present []string
		absent  []string
	}{
		{"skills-mgr ", []string{"get", "set", "completion"}, nil},
		{"skills-mgr completion ", []string{"fish"}, nil},
		{"skills-mgr get ", []string{"alpha"}, []string{"beta"}},
		{"skills-mgr get --", []string{"--codex"}, nil},
		{"skills-mgr run ", []string{"alpha/"}, []string{"beta/"}},
		{"skills-mgr set ", []string{"alpha", "beta"}, nil},
		{"skills-mgr set -", []string{"-g"}, nil},
		{"skills-mgr set alpha ", []string{"true", "false", "inherit"}, []string{"beta"}},
		{"skills-mgr edit alpha --file ", nil, []string{"alpha"}},
		{"skills-mgr set run ", []string{"true"}, []string{"--codex"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			cmd := exec.Command(fish, "--no-config", "-c", "source completion.fish; complete -C $argv[1]", "--", tc.command)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fish completion: %v: %s", err, output)
			}
			var values []string
			for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
				value, _, _ := strings.Cut(line, "\t")
				values = append(values, value)
			}
			for _, want := range tc.present {
				if !slices.Contains(values, want) {
					t.Errorf("missing %q in %q", want, output)
				}
			}
			for _, unwanted := range tc.absent {
				if slices.Contains(values, unwanted) {
					t.Errorf("unexpected %q in %q", unwanted, output)
				}
			}
		})
	}
}
