package main

import (
	"context"

	"mvdan.cc/sh/v3/interp"
)

func enabledCallHandlerWithEvidence(
	evidence *projectEvidenceIndex,
) interp.CallHandlerFunc {
	handlers := [...]interp.CallHandlerFunc{
		dependencyCallHandler(evidence),
		languageCallHandler(evidence),
		toolingCallHandler(evidence),
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
