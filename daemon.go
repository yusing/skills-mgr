package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const remoteRefreshInterval = 5 * time.Minute

func runDaemon(ctx context.Context, registry *remoteRegistry, stderr io.Writer) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	refreshRemoteRegistry(ctx, registry, stderr)
	ticker := time.NewTicker(remoteRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			refreshRemoteRegistry(ctx, registry, stderr)
		}
	}
}

func refreshRemoteRegistry(ctx context.Context, registry *remoteRegistry, stderr io.Writer) {
	if err := registry.refresh(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "skills-mgr: refresh remote registry: %v\n", err)
	}
}
