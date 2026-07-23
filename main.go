package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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
	skillsMP := newSkillsMPRegistry(paths.skillsMP, os.Getenv("SKILLSMP_API_KEY"))
	manager := &manager{paths: paths, skillsMP: skillsMP}

	switch {
	case len(args) == 0:
		project, err := currentProject()
		if err != nil {
			return err
		}
		return runTUI(manager, project)
	case args[0] == "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: skills-mgr list")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		return manager.list(project, os.Stdout)
	case args[0] == "get":
		if len(args) != 2 && len(args) != 3 {
			return fmt.Errorf("usage: skills-mgr get <skill-name>[/relative/path] [start:end]")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		lineRange := ""
		if len(args) == 3 {
			lineRange = args[2]
		}
		return manager.get(project, args[1], lineRange, os.Stdout)
	case args[0] == "run":
		if len(args) < 2 {
			return fmt.Errorf("usage: skills-mgr run <skill-name>/<relative/script> [args...]")
		}
		project, err := currentProject()
		if err != nil {
			return err
		}
		command, err := manager.scriptCommand(project, args[1], args[2:])
		if err != nil {
			return err
		}
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		return command.Run()
	case len(args) == 1 && args[0] == "daemon":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemon(ctx, newRemoteRegistry(paths.remoteRegistry), os.Stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func currentProject() (string, error) {
	project, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	return project, nil
}
