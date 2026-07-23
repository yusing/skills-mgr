package main

import "context"

func runDaemon(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
