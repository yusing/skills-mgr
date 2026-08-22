package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
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
	paths, err := defaultPaths()
	if err != nil {
		return err
	}
	if err := paths.relocateGlobalLock(); err != nil {
		return err
	}
	global := len(args) == 1 && args[0] == "-g"
	remote := newRemoteRegistry(paths.remoteRegistry)
	skillsMP := newSkillsMPRegistry(paths.skillsMP, os.Getenv("SKILLSMP_API_KEY"))
	manager := &manager{
		paths: paths, remote: remote, skillsMP: skillsMP,
		remoteStore: newRemoteSkillStore(paths.remoteSkills),
		global:      global,
	}

	switch {
	case len(args) == 0 || global:
		project, err := currentProject()
		if err != nil {
			return err
		}
		return runTUI(manager, project)
	case args[0] == "help":
		if len(args) != 1 {
			return fmt.Errorf("usage: skills-mgr help")
		}
		_, err = fmt.Fprint(os.Stdout, `Usage:
  skills-mgr
  skills-mgr -g
  skills-mgr help
  skills-mgr list [--claude] [--grok] [--codex]
  skills-mgr sync
  skills-mgr get [--claude] [--grok] [--codex] <skill-name>[/relative/path] [start:end]
  skills-mgr run [--claude] [--grok] [--codex] <skill-name>/<relative/script> [args...]
  skills-mgr daemon [refresh|sync]
`)
		return err
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
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return manager.listContext(ctx, project, os.Stdout, resolveHarnesses(harnesses)...)
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
		return command.Run()
	case args[0] == "daemon":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		switch {
		case len(args) == 1:
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			return runDaemon(ctx, manager, logger)
		case len(args) == 2 && (args[1] == daemonCommandRefresh || args[1] == daemonCommandSync):
			return triggerDaemon(ctx, manager.paths.daemonSocket, args[1])
		default:
			return fmt.Errorf("usage: skills-mgr daemon [refresh|sync]")
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
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

func resolveHarnesses(explicit []listHarness) []listHarness {
	if len(explicit) > 0 {
		return explicit
	}
	return inferHarnessFromEnv()
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

func currentProject() (string, error) {
	project, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	return project, nil
}
