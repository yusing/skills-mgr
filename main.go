package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

type scriptExitError struct{ *exec.ExitError }

func (e *scriptExitError) Unwrap() error { return e.ExitError }

func main() {
	if err := run(os.Args[1:]); err != nil {
		if exitError, ok := errors.AsType[*scriptExitError](err); ok {
			code := exitError.ExitCode()
			if code < 0 {
				code = 1
			}
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, "skills-mgr:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help") {
		if len(args) > 2 || (args[0] == "--help" && len(args) != 1) {
			return fmt.Errorf("usage: skills-mgr help [command]")
		}
		if len(args) == 2 {
			if args[1] == "--help" {
				return printHelp("help")
			}
			return printHelp(args[1])
		}
		return printHelp("")
	}
	if len(args) == 2 && args[1] == "--help" {
		return printHelp(args[0])
	}
	if len(args) == 2 && args[0] == "completion" && args[1] == "fish" {
		_, err := fmt.Fprint(os.Stdout, fishCompletion)
		return err
	}
	paths, err := defaultPaths()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "info" {
		if len(args) != 1 {
			return fmt.Errorf("usage: skills-mgr info")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		return printInfo(os.Stdout, paths, project)
	}
	global := len(args) == 1 && args[0] == "-g"
	remote := newRemoteRegistry(paths.remoteRegistry)
	skillsMP := newSkillsMPRegistry(paths.skillsMP, os.Getenv("SKILLSMP_API_KEY"))
	manager := &manager{
		paths: paths, remote: remote, skillsMP: skillsMP,
		remoteStore: newRemoteSkillStore(
			paths.remoteSkills,
			filepath.Join(paths.managedSkills, remoteSkillPatchDir),
		),
		global: global,
	}
	if len(args) == 2 && args[0] == "__complete-skills" && (args[1] == "all" || args[1] == "enabled") {
		project, err := currentProject()
		if err != nil {
			return err
		}
		return manager.completeSkills(context.Background(), project, args[1] == "enabled", os.Stdout)
	}
	if err := paths.relocateGlobalLock(); err != nil {
		return err
	}
	if os.Getenv("SKILLS_MGR_NO_REFRESH") != "1" && (len(args) == 0 || args[0] != refreshRunnerCommand) {
		maybeStartRefreshRunner(manager)
	}

	switch {
	case len(args) == 0 || global:
		project, err := currentProject()
		if err != nil {
			return err
		}
		if err := manager.useGlobalAtHome(project); err != nil {
			return err
		}
		return runTUI(manager, project)
	case args[0] == "completion":
		return fmt.Errorf("usage: skills-mgr completion fish")
	case args[0] == "inspect" || args[0] == "check" || args[0] == "set" || args[0] == "edit":
		project, err := currentProject()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return manager.agentCommand(ctx, project, args, os.Stdin, os.Stdout, os.Stderr)
	case args[0] == "adopt":
		if len(args) != 1 {
			return fmt.Errorf("usage: skills-mgr adopt")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		return manager.adoptSharedSkills(project, os.Stdout)
	case args[0] == "list":
		harnesses, rest, err := parseHarnessArgs(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return fmt.Errorf("usage: skills-mgr list [--claude] [--grok] [--codex]")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		if len(harnesses) == 0 {
			harnesses = inferHarnessFromEnv()
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return manager.listContext(ctx, project, os.Stdout, harnesses...)
	case args[0] == "sync":
		if len(args) != 1 {
			return fmt.Errorf("usage: skills-mgr sync")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return manager.sync(ctx, project, os.Stdout)
	case args[0] == "install":
		const usage = "usage: skills-mgr install [-g] [--exclude <pattern>]... <repo-address> [skill-name]"
		rest := args[1:]
		var exclude []string
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			switch {
			case rest[0] == "-g":
				manager.global = true
				rest = rest[1:]
			case rest[0] == "--exclude" && len(rest) > 1:
				exclude = append(exclude, rest[1])
				rest = rest[2:]
			default:
				return errors.New(usage)
			}
		}
		if len(rest) < 1 || len(rest) > 2 {
			return errors.New(usage)
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		if err := manager.useGlobalAtHome(project); err != nil {
			return err
		}
		name := ""
		if len(rest) == 2 {
			name = rest[1]
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return manager.installRepository(ctx, project, rest[0], name, exclude, os.Stdout, os.Stderr)
	case args[0] == "get":
		_, rest, err := parseHarnessArgs(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 && len(rest) != 2 {
			return fmt.Errorf("usage: skills-mgr get [--claude] [--grok] [--codex] <skill-name>[/relative/path] [start:end]")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		lineRange := ""
		if len(rest) == 2 {
			lineRange = rest[1]
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return manager.getContext(ctx, project, rest[0], lineRange, os.Stdout)
	case args[0] == "run":
		_, rest, err := parseHarnessArgs(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 1 {
			return fmt.Errorf("usage: skills-mgr run [--claude] [--grok] [--codex] <skill-name>/<relative/script> [args...]")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		command, err := manager.scriptCommandContext(ctx, project, rest[0], rest[1:])
		if err != nil {
			return err
		}
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
				return &scriptExitError{exitError}
			}
			return err
		}
		return nil
	case args[0] == refreshRunnerCommand:
		if len(args) != 1 {
			return fmt.Errorf("usage: skills-mgr %s", refreshRunnerCommand)
		}
		return executeRefreshRunner(manager)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

var commandHelp = []struct {
	name, usage, description string
}{
	{"help", "help [command] | --help", "Show all commands or help for one command.\n    Each command also accepts --help on its own."},
	{"completion", "completion fish", "Print fish shell completions."},
	{"info", "info", "Show metadata and skill paths; mark missing paths."},
	{"adopt", "adopt", "Move shared user skills into the managed skill store."},
	{"list", "list [--claude] [--grok] [--codex]", "Print enabled skill metadata as XML."},
	{"sync", "sync", "Fetch globally listed remote skills and this project's enabled ones."},
	{"install", "install [-g] [--exclude <pattern>]... <repo-address> [skill-name]", "Install a repository skill and enable it for this project, or globally with -g.\n    --exclude leaves out matching files and directories on this and later fetches."},
	{"get", "get [--claude] [--grok] [--codex] <skill-name>[/relative/path] [start:end]", "Print an enabled skill or file, optionally limited to an inclusive 1-based line range.\n    Lines refer to the served body after frontmatter removal and any remote patch.\n    Examples: skills-mgr get example 1:120\n              skills-mgr get example 121:240"},
	{"inspect", "inspect <skill>", "Print JSON owner, selection, fallback, patch, and body-health diagnostics."},
	{"check", "check <target> [target...]", "Validate served bodies and a skill's listed references without running scripts."},
	{"set", "set [-g] <skill> <true|false|condition|inherit>", "Set a selection value, or remove its override with inherit."},
	{"edit", "edit [-g] <skill> --file <path|-> [--expect-sha256 <digest>]", "Replace a complete SKILL.md, preserving remote ownership with a local patch."},
	{"run", "run [--claude] [--grok] [--codex] <skill-name>/<relative/script> [args...]", "Run a script from an enabled skill, forwarding arguments."},
}

func printHelp(target string) error {
	var output strings.Builder
	output.WriteString("Usage:\n")
	if target == "" {
		output.WriteString("  skills-mgr [-g]\n    Open the project skill selection interface, or global with -g.\n")
	}
	for _, command := range commandHelp {
		if target == "" || target == command.name {
			fmt.Fprintf(&output, "\n  skills-mgr %s\n    %s\n", command.usage, command.description)
			if target != "" {
				_, err := fmt.Fprint(os.Stdout, output.String())
				return err
			}
		}
	}
	if target != "" {
		return fmt.Errorf("unknown help target %q", target)
	}
	_, err := fmt.Fprint(os.Stdout, output.String())
	return err
}

func parseHarnessArgs(args []string) ([]listHarness, []string, error) {
	harnesses := make([]listHarness, 0, len(args))
	for index, arg := range args {
		switch arg {
		case "--claude":
			harnesses = append(harnesses, listHarnessClaude)
		case "--grok":
			harnesses = append(harnesses, listHarnessGrok)
		case "--codex":
			harnesses = append(harnesses, listHarnessCodex)
		default:
			if strings.HasPrefix(arg, "--") {
				return nil, nil, fmt.Errorf("unknown flag %q", arg)
			}
			return harnesses, args[index:], nil
		}
	}
	return harnesses, nil, nil
}

func inferHarnessFromEnv() []listHarness {
	var detected []listHarness
	if os.Getenv("CLAUDECODE") != "" {
		detected = append(detected, listHarnessClaude)
	}
	if os.Getenv("GROK_AGENT") != "" || os.Getenv("GROK_SESSION_ID") != "" {
		detected = append(detected, listHarnessGrok)
	}
	if os.Getenv("CODEX_THREAD_ID") != "" {
		detected = append(detected, listHarnessCodex)
	}
	if len(detected) != 1 {
		return nil
	}
	return detected
}

// useGlobalAtHome switches a selection-writing command to the global layer
// when it runs from $HOME or the manager home, as -g would.
func (m *manager) useGlobalAtHome(project string) error {
	atHome, err := m.paths.atHome(project)
	if err != nil {
		return err
	}
	m.global = m.global || atHome
	return nil
}

func currentProject() (string, error) {
	project, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	return project, nil
}
