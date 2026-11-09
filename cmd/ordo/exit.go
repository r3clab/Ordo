package main

import (
	"context"
	"errors"

	"github.com/r3clab/Ordo/internal/server"
)

// Exit statuses are part of the command-line contract, which README.md documents.
const (
	exitUnsuccessful = 1
	exitRefused      = 2
	exitUnavailable  = 3
	exitInterrupted  = 130
)

type exitError struct {
	status int
	err    error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func exitStatus(err error) int {
	if e, ok := errors.AsType[*exitError](err); ok {
		return e.status
	}
	switch {
	case errors.Is(err, context.Canceled):
		return exitInterrupted
	case errors.Is(err, server.ErrUnavailable):
		return exitUnavailable
	}
	return exitRefused
}
