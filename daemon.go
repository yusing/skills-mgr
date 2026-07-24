package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const remoteRefreshInterval = 5 * time.Minute

func runDaemon(ctx context.Context, manager *manager, stderr io.Writer) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	refreshRemoteRegistry(ctx, manager.remote, stderr)
	refreshPersistedRemoteSkills(ctx, manager, stderr)
	ticker := time.NewTicker(remoteRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			refreshRemoteRegistry(ctx, manager.remote, stderr)
			refreshPersistedRemoteSkills(ctx, manager, stderr)
		}
	}
}

func refreshRemoteRegistry(ctx context.Context, registry *remoteRegistry, stderr io.Writer) {
	if err := registry.refresh(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "skills-mgr: refresh remote registry: %v\n", err)
	}
}

func refreshPersistedRemoteSkills(ctx context.Context, manager *manager, stderr io.Writer) {
	if manager == nil || manager.remoteStore == nil {
		return
	}
	records, err := manager.remoteStore.records()
	if err != nil {
		fmt.Fprintf(stderr, "skills-mgr: inspect persisted remote skills: %v\n", err)
		return
	}
	now := time.Now()
	for _, record := range records {
		if record.fresh(now) {
			continue
		}
		provider := manager.remoteContentProvider(record.Provider)
		err := manager.remoteStore.refresh(ctx, record, provider)
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(
				stderr,
				"skills-mgr: refresh persisted remote skill %s:%s: %v\n",
				record.Provider,
				record.ID,
				err,
			)
		}
	}
}
