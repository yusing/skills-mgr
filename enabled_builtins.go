package main

import (
	"context"

	"mvdan.cc/sh/v3/interp"
)

func enabledCallHandler(project string) interp.CallHandlerFunc {
	handlers := [...]interp.CallHandlerFunc{
		dependencyCallHandler(project),
		languageCallHandler(project),
	}
	return func(ctx context.Context, args []string) ([]string, error) {
		var err error
		for _, handler := range handlers {
			args, err = handler(ctx, args)
			if err != nil {
				return nil, err
			}
		}
		return args, nil
	}
}
