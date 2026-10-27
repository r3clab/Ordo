package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/r3clab/Ordo/internal/server"
)

var stdin io.Reader = os.Stdin

const submitUsage = "Usage: ordo submit [--state DIR] [--file JOBS.jsonl] [--wait]\n\nReads one job per line (stdin by default) and prints one result per line. Relative out and input\npaths are taken from the current directory. A line whose out already holds results is done and\nqueues nothing; resubmitting a line whose job is unfinished returns that job. --wait then prints each\njob's line again with its final state as it finishes, and fails if any did not succeed."

func submitCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("submit")
	var root, file string
	var wait bool
	stateFlag(f, &root)
	f.StringVar(&file, "file", "-", "JSONL jobs, or - for stdin")
	f.BoolVar(&wait, "wait", false, "wait for the submitted jobs to finish")
	if err := parse(f, args, out, submitUsage); err != nil {
		return err
	}
	if root == "" || f.NArg() != 0 {
		return errors.New("--state is required")
	}
	input := stdin
	if file != "-" {
		jobs, err := os.Open(file)
		if err != nil {
			return err
		}
		defer func() { _ = jobs.Close() }()
		input = jobs
	}
	body, err := absolutize(input)
	if err != nil {
		return err
	}
	client := server.Client(server.Socket(root))
	defer client.CloseIdleConnections()
	resp, err := server.Send(ctx, client, http.MethodPost, "/v1/batches", "application/x-ndjson", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	decoder := json.NewDecoder(resp.Body)
	encoder := json.NewEncoder(out)
	total, failed := 0, 0
	var queued []server.BatchResult
	for {
		var result server.BatchResult
		if err := decoder.Decode(&result); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		total++
		if result.ID != "" {
			queued = append(queued, result)
		}
		if result.Error != "" {
			failed++
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d lines failed to submit", failed, total)
	}
	if wait {
		return await(ctx, client, encoder, queued)
	}
	return nil
}

var pollInterval = 2 * time.Second

// await prints each queued line again once its job ends, in the order they end.
func await(ctx context.Context, client *http.Client, encoder *json.Encoder, queued []server.BatchResult) error {
	pending := make(map[string]server.BatchResult, len(queued))
	for _, result := range queued {
		pending[result.ID] = result
	}
	unsuccessful := 0
	for len(pending) > 0 {
		query := url.Values{}
		for id := range pending {
			query.Add("id", id)
		}
		jobs, err := listJobs(ctx, client, query)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			result, ok := pending[j.ID]
			if !ok || !j.State.Terminal() {
				continue
			}
			delete(pending, j.ID)
			result.State, result.Error, result.Existing = j.State, j.Error, false
			if j.State != server.Succeeded {
				unsuccessful++
			}
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if unsuccessful > 0 {
		return fmt.Errorf("%d of %d jobs did not succeed", unsuccessful, len(queued))
	}
	return nil
}

// absolutize resolves each line's relative out and input paths here, as the server knows nothing of
// this directory. It keeps every line, so the server's line numbers still match; one it cannot parse
// goes through unchanged for the server to report.
func absolutize(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, server.MaxBatchLine)
	var body bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) == nil {
			resolved, err := resolvePaths(fields)
			if err != nil {
				return nil, err
			}
			line = resolved
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	return body.Bytes(), scanner.Err()
}

func resolvePaths(fields map[string]json.RawMessage) ([]byte, error) {
	var out string
	if json.Unmarshal(fields["out"], &out) == nil && out != "" {
		abs, err := filepath.Abs(out)
		if err != nil {
			return nil, err
		}
		fields["out"], _ = json.Marshal(abs)
	}
	var inputs map[string]string
	if json.Unmarshal(fields["inputs"], &inputs) == nil && inputs != nil {
		for name, source := range inputs {
			abs, err := filepath.Abs(source)
			if err != nil {
				return nil, err
			}
			inputs[name] = abs
		}
		fields["inputs"], _ = json.Marshal(inputs)
	}
	return json.Marshal(fields)
}
