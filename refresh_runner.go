package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	remoteRefreshInterval = 5 * time.Minute
	refreshRunnerCommand  = "refresh-runner"
	refreshRunnerTrigger  = "ondemand"
)

var (
	startBackgroundRefresh = startDetachedRefreshRunner
	executeRefreshRunner   = runRefreshRunnerCommand
)

// maybeStartRefreshRunner starts a detached refresh child when no runner
// currently holds the lock. The parent only probes the lock; the child holds
// it for the duration of the work.
func maybeStartRefreshRunner(manager *manager) {
	if manager == nil {
		return
	}
	file, err := tryFlockExclusive(manager.paths.refreshLock)
	if err != nil {
		if !errors.Is(err, errAlreadyLocked) {
			logRefreshRunnerSpawnError(manager.paths.refreshLog, err)
		}
		return
	}
	closeExclusiveLock(file)
	if err := startBackgroundRefresh(manager); err != nil {
		logRefreshRunnerSpawnError(manager.paths.refreshLog, err)
	}
}

func startDetachedRefreshRunner(manager *manager) error {
	if manager == nil {
		return fmt.Errorf("refresh runner manager is unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find skills-mgr executable: %w", err)
	}
	logFile, err := openRefreshLog(manager.paths.refreshLog)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, refreshRunnerCommand)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start refresh runner: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func runRefreshRunnerCommand(manager *manager) error {
	if manager == nil {
		return nil
	}
	logFile, err := openRefreshLog(manager.paths.refreshLog)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, nil))
	return runRefreshRunner(context.Background(), manager, logger)
}

func openRefreshLog(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("refresh log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create refresh log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open refresh log %s: %w", path, err)
	}
	return file, nil
}

func logRefreshRunnerSpawnError(path string, err error) {
	file, openErr := openRefreshLog(path)
	if openErr != nil {
		return
	}
	defer file.Close()
	slog.New(slog.NewTextHandler(file, nil)).Error("start refresh runner", "err", err)
}

func runRefreshRunner(
	ctx context.Context,
	manager *manager,
	logger *slog.Logger,
) error {
	if ctx.Err() != nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if manager == nil {
		return nil
	}
	lockFile, err := tryFlockExclusive(manager.paths.refreshLock)
	if err != nil {
		if errors.Is(err, errAlreadyLocked) {
			return nil
		}
		return err
	}
	defer closeExclusiveLock(lockFile)
	if registryRefreshDue(manager.remote, time.Now()) {
		_ = refreshRemoteRegistry(ctx, manager.remote, logger)
	} else if manager.remote != nil {
		logger.Debug(
			"skipping registry cache refresh",
			"trigger", refreshRunnerTrigger,
			"reason", "fresh",
		)
	}
	if ctx.Err() != nil {
		return nil
	}
	_ = refreshPersistedRemoteSkills(ctx, manager, logger)
	return nil
}

func registryRefreshDue(registry *remoteRegistry, now time.Time) bool {
	if registry == nil {
		return false
	}
	cache, err := loadRemoteCache(registry.cachePath)
	if err != nil || cache.UpdatedAt.IsZero() {
		return true
	}
	return !now.Before(cache.UpdatedAt.Add(remoteRefreshInterval))
}

func refreshRemoteRegistry(
	ctx context.Context,
	registry *remoteRegistry,
	logger *slog.Logger,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	if registry == nil {
		logger.Debug(
			"skipping registry cache refresh",
			"trigger", refreshRunnerTrigger,
			"reason", "no registry",
		)
		return nil
	}
	logger.Info("refreshing registry cache", "trigger", refreshRunnerTrigger)
	start := time.Now()
	if err := registry.refresh(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error(
				"registry cache refresh failed",
				"err", err,
				"trigger", refreshRunnerTrigger,
			)
		}
		return err
	}
	logger.Info(
		"refreshed registry cache",
		"trigger", refreshRunnerTrigger,
		"duration", time.Since(start),
	)
	return nil
}

func refreshPersistedRemoteSkills(
	ctx context.Context,
	manager *manager,
	logger *slog.Logger,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	if manager == nil || manager.remoteStore == nil {
		logger.Debug(
			"skipping persisted remote skill update",
			"trigger", refreshRunnerTrigger,
			"reason", "no store",
		)
		return nil
	}
	start := time.Now()
	records, err := manager.remoteStore.records()
	if err != nil {
		logger.Error(
			"inspect persisted remote skills failed",
			"err", err,
			"trigger", refreshRunnerTrigger,
		)
		return err
	}
	now := time.Now()
	var updated, skipped, failed int
	var errs error
	started := false
	for _, record := range records {
		if record.fresh(now) {
			skipped++
			logger.Debug(
				"skipping fresh remote skill",
				"provider", record.Provider,
				"id", record.ID,
				"name", record.Name,
			)
			continue
		}
		if !started {
			logger.Info("updating persisted remote skills", "trigger", refreshRunnerTrigger)
			started = true
		}
		logger.Info(
			"updating remote skill",
			"provider", record.Provider,
			"id", record.ID,
			"name", record.Name,
		)
		err := manager.remoteStore.refresh(
			ctx, record, manager.remoteContentProvider(record.Provider),
		)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			failed++
			logger.Error(
				"remote skill update failed",
				"err", err,
				"provider", record.Provider,
				"id", record.ID,
				"name", record.Name,
				"trigger", refreshRunnerTrigger,
			)
			errs = errors.Join(errs, fmt.Errorf("%s:%s: %w", record.Provider, record.ID, err))
			continue
		}
		updated++
		logger.Info(
			"updated remote skill",
			"provider", record.Provider,
			"id", record.ID,
			"name", record.Name,
		)
	}
	finished := logger.Debug
	if updated > 0 || failed > 0 {
		finished = logger.Info
	}
	finished(
		"persisted remote skill update finished",
		"trigger", refreshRunnerTrigger,
		"updated", updated,
		"skipped", skipped,
		"failed", failed,
		"duration", time.Since(start),
	)
	return errs
}
