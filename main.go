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
		fmt.Fprintln(os.Stderr, "skill-mgr:", err)
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
		project, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
		return runTUI(manager, project)
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
		return runDaemon(ctx, manager, os.Stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
