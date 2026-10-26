package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/worker"
)

var stdin io.Reader = os.Stdin

const submitUsage = "Usage: ordo submit --state DIR [--file JOBS.jsonl]\n\nReads one job request per line (stdin by default) and prints one result per line.\nEach job needs an id; resubmitting an id with the same request returns the existing job."

func submitCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("submit")
	var root, file string
	f.StringVar(&root, "state", "", "server state directory")
	f.StringVar(&file, "file", "-", "JSONL job requests, or - for stdin")
	if err := parse(f, args, out, submitUsage); err != nil {
		return err
	}
	if root == "" || f.NArg() != 0 {
		return errors.New("--state is required")
	}
	body := stdin
	if file != "-" {
		jobs, err := os.Open(file)
		if err != nil {
			return err
		}
		defer func() { _ = jobs.Close() }()
		body = jobs
	}
	client := serverClient(root)
	defer client.CloseIdleConnections()
	resp, err := worker.Send(ctx, client, http.MethodPost, "/v1/batches", "application/x-ndjson", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	decoder := json.NewDecoder(resp.Body)
	encoder := json.NewEncoder(out)
	total, failed := 0, 0
	for {
		var result server.BatchResult
		if err := decoder.Decode(&result); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		total++
		if result.Error != "" {
			failed++
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d jobs failed to submit", failed, total)
	}
	return nil
}
