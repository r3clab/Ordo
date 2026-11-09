package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/r3clab/Ordo/internal/server"
)

func TestExitStatusClassifiesErrors(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{errors.New("--state is required"), exitRefused},
		{&server.StatusError{Code: 404, Body: "no job"}, exitRefused},
		{fmt.Errorf("get: %w", &server.StatusError{Code: 500, Body: "disk full"}), exitUnavailable},
		{server.Unanswered(errors.New("connection reset")), exitUnavailable},
		{server.Unanswered(context.Canceled), exitInterrupted},
		{&exitError{exitUnsuccessful, errors.New("1 of 1 jobs did not succeed")}, exitUnsuccessful},
		{&exitError{7, nil}, 7},
	} {
		if got := exitStatus(test.err); got != test.status {
			t.Errorf("exitStatus(%v) = %d, want %d", test.err, got, test.status)
		}
	}
	if (&exitError{7, nil}).Error() != "" {
		t.Fatal("a passed-on status must print nothing")
	}
}

func TestSubmitWithoutServerIsUnavailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "server")
	_, err := runWith(t, `{"out":"/tmp/x","image":"/src#image","resources":{"cores":1,"memory_mib":128}}`, "submit", "--state", root)
	if status := exitStatus(err); status != exitUnavailable {
		t.Fatalf("status %d for %v", status, err)
	}
}
