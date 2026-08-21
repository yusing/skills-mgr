package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	remoteRefreshInterval = 5 * time.Minute
	daemonCommandRefresh  = "refresh"
	daemonCommandSync     = "sync"
	daemonCommandDeadline = 5 * time.Second
)

type daemon struct {
	manager *manager
	logger  *slog.Logger
	mu      sync.Mutex
}

func runDaemon(ctx context.Context, manager *manager, logger *slog.Logger) error {
	return runDaemonReady(ctx, manager, logger, nil)
}

func runDaemonReady(
	ctx context.Context,
	manager *manager,
	logger *slog.Logger,
	ready chan<- struct{},
) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	if logger == nil {
		logger = slog.Default()
	}
	socket := ""
	if manager != nil {
		socket = manager.paths.daemonSocket
	}
	listener, err := listenDaemonSocket(socket)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	}()

	d := &daemon{manager: manager, logger: logger}
	logger.Info("daemon started", "socket", socket, "interval", remoteRefreshInterval)

	var wg sync.WaitGroup
	wg.Go(func() {
		<-ctx.Done()
		_ = listener.Close()
	})
	wg.Go(func() {
		d.acceptLoop(ctx, listener)
	})
	wg.Go(func() {
		d.timerLoop(ctx)
	})
	if ready != nil {
		close(ready)
	}

	d.runCycle(ctx, "startup")
	<-ctx.Done()
	wg.Wait()
	logger.Info("daemon stopped")
	return nil
}

func listenDaemonSocket(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("daemon socket path is empty")
	}
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("daemon already running")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale daemon socket %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create daemon socket directory: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("restrict daemon socket %s: %w", path, err)
	}
	return listener, nil
}

func (d *daemon) timerLoop(ctx context.Context) {
	ticker := time.NewTicker(remoteRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runCycle(ctx, "timer")
		}
	}
}

func (d *daemon) runCycle(ctx context.Context, trigger string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil || d.manager == nil {
		return
	}
	_ = refreshRemoteRegistry(ctx, d.manager.remote, d.logger, trigger)
	if ctx.Err() != nil {
		return
	}
	_ = refreshPersistedRemoteSkills(ctx, d.manager, d.logger, trigger)
}

func (d *daemon) acceptLoop(ctx context.Context, listener net.Listener) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			d.logger.Warn("accept daemon connection", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		wg.Go(func() {
			d.serveConn(ctx, conn)
		})
	}
}

func (d *daemon) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(daemonCommandDeadline)); err != nil {
		d.logger.Warn("set daemon connection deadline", "err", err)
		return
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	command := strings.TrimSpace(scanner.Text())
	if err := conn.SetDeadline(time.Time{}); err != nil {
		d.logger.Warn("clear daemon connection deadline", "err", err)
		return
	}

	d.logger.Info("received daemon command", "command", command)
	d.mu.Lock()
	var (
		err      error
		registry *remoteRegistry
	)
	if d.manager != nil {
		registry = d.manager.remote
	}
	switch command {
	case daemonCommandRefresh:
		err = refreshRemoteRegistry(ctx, registry, d.logger, "command")
	case daemonCommandSync:
		err = refreshPersistedRemoteSkills(ctx, d.manager, d.logger, "command")
	default:
		err = fmt.Errorf("unknown command %q", command)
		d.logger.Warn("rejected daemon command", "command", command)
	}
	d.mu.Unlock()
	if ctx.Err() != nil {
		return
	}

	if deadlineErr := conn.SetDeadline(time.Now().Add(daemonCommandDeadline)); deadlineErr != nil {
		d.logger.Warn("set daemon response deadline", "err", deadlineErr)
		return
	}
	if err != nil {
		fmt.Fprintf(conn, "error %s\n", sanitizeDaemonLine(err.Error()))
		return
	}
	fmt.Fprintln(conn, "ok")
}

func triggerDaemon(ctx context.Context, socket, command string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		if isDaemonUnavailable(err) {
			return fmt.Errorf("daemon is not running")
		}
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set daemon request deadline: %w", err)
		}
	}
	if _, err := fmt.Fprintln(conn, command); err != nil {
		return fmt.Errorf("send daemon %s: %w", command, err)
	}
	stopClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stopClose()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read daemon %s response: %w", command, err)
		}
		return fmt.Errorf("daemon closed %s without a response", command)
	}
	line := scanner.Text()
	switch {
	case line == "ok":
		return nil
	case strings.HasPrefix(line, "error "):
		return errors.New(strings.TrimPrefix(line, "error "))
	default:
		return fmt.Errorf("unexpected daemon response %q", line)
	}
}

func isDaemonUnavailable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOTSOCK) ||
		errors.Is(err, syscall.ECONNRESET)
}

func sanitizeDaemonLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

func refreshRemoteRegistry(
	ctx context.Context,
	registry *remoteRegistry,
	logger *slog.Logger,
	trigger string,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	if registry == nil {
		logger.Info("skipping registry cache refresh", "trigger", trigger, "reason", "no registry")
		return nil
	}
	logger.Info("refreshing registry cache", "trigger", trigger)
	start := time.Now()
	if err := registry.refresh(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("registry cache refresh failed", "err", err, "trigger", trigger)
		}
		return err
	}
	logger.Info("refreshed registry cache", "trigger", trigger, "duration", time.Since(start))
	return nil
}

func refreshPersistedRemoteSkills(
	ctx context.Context,
	manager *manager,
	logger *slog.Logger,
	trigger string,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	if manager == nil || manager.remoteStore == nil {
		logger.Info("skipping persisted remote skill update", "trigger", trigger, "reason", "no store")
		return nil
	}
	logger.Info("updating persisted remote skills", "trigger", trigger)
	start := time.Now()
	records, err := manager.remoteStore.records()
	if err != nil {
		logger.Error("inspect persisted remote skills failed", "err", err, "trigger", trigger)
		return err
	}
	now := time.Now()
	var updated, skipped, failed int
	var errs error
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
				"trigger", trigger,
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
	logger.Info(
		"persisted remote skill update finished",
		"trigger", trigger,
		"updated", updated,
		"skipped", skipped,
		"failed", failed,
		"duration", time.Since(start),
	)
	return errs
}
