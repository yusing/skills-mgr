package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
)

//go:embed completion.fish
var fishCompletion string

func (m *manager) completeSkills(ctx context.Context, project string, enabledOnly bool, out io.Writer) error {
	skills, err := m.skills(project)
	if err != nil {
		return err
	}
	var selection selectionState
	if enabledOnly {
		selection, err = m.selectionState(project, skills)
		if err != nil {
			return err
		}
	}
	evaluator := newEnabledEvaluator(project)
	for _, skill := range skills {
		if enabledOnly {
			enabled, err := selection.enabled(ctx, evaluator, skill.Name)
			if err != nil {
				return err
			}
			if !enabled {
				continue
			}
		}
		if _, err := fmt.Fprintln(out, skill.Name); err != nil {
			return err
		}
	}
	return nil
}
