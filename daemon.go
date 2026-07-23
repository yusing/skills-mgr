package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

func runDaemon(ctx context.Context, manager *manager, output io.Writer) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	added := make(map[string]bool)
	addWatches := func() error {
		skills, err := manager.skills()
		if err != nil {
			return err
		}
		roots := []string{manager.paths.library}
		for _, skill := range skills {
			root, err := filepath.EvalSymlinks(filepath.Join(manager.paths.library, skill))
			if err != nil {
				return err
			}
			roots = append(roots, root)
		}
		for _, root := range roots {
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() && !added[path] {
					if err := watcher.Add(path); err != nil {
						return err
					}
					added[path] = true
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := addWatches(); err != nil {
		return err
	}
	fmt.Fprintf(output, "Watching %s\n", manager.paths.library)

	var timer *time.Timer
	var ready <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			fmt.Fprintf(output, "Changed %s\n", event.Name)
			if timer == nil {
				timer = time.NewTimer(150 * time.Millisecond)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(150 * time.Millisecond)
			}
			ready = timer.C
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(output, "Watch error: %v\n", err)
		case <-ready:
			ready = nil
			if err := addWatches(); err != nil {
				fmt.Fprintf(output, "Watch error: %v\n", err)
			}
			count, err := manager.syncAll()
			if err != nil {
				fmt.Fprintf(output, "Sync error: %v\n", err)
			} else {
				fmt.Fprintf(output, "Updated %d skill installations\n", count)
			}
		}
	}
}
