package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "skills-mgr:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	paths, err := defaultPaths()
	if err != nil {
		return err
	}
	manager := &manager{paths: paths}

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
	case len(args) == 1 && args[0] == "migrate":
		count, err := manager.migrate()
		if err != nil {
			return err
		}
		fmt.Printf("Managed %d skills in %s\n", count, paths.library)
		return nil
	case len(args) == 1 && args[0] == "daemon":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemon(ctx)
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
