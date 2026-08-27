package main

import (
	"errors"
	"slices"
)

type mutationJournal struct {
	undos []func() error
}

func (j *mutationJournal) add(undo func() error) {
	if undo != nil {
		j.undos = append(j.undos, undo)
	}
}

func (j *mutationJournal) rollback() error {
	undos := j.undos
	j.undos = nil
	var rollbackErr error
	for _, undo := range slices.Backward(undos) {
		rollbackErr = errors.Join(rollbackErr, undo())
	}
	return rollbackErr
}

func (j *mutationJournal) undo() func() error {
	if len(j.undos) == 0 {
		return nil
	}
	return j.rollback
}
