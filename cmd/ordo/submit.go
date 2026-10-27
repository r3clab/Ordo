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
	"os"
	"path/filepath"

	"github.com/r3clab/Ordo/internal/server"
)

var stdin io.Reader = os.Stdin

const submitUsage = "Usage: ordo submit [--state DIR] [--file JOBS.jsonl]\n\nReads one job per line (stdin by default) and prints one result per line. Relative out and input\npaths are taken from the current directory. A line whose out already holds results is done and\nqueues nothing; resubmitting a line whose job is unfinished returns that job."

func submitCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("submit")
	var root, file string
	stateFlag(f, &root)
	f.StringVar(&file, "file", "-", "JSONL jobs, or - for stdin")
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
		return fmt.Errorf("%d of %d lines failed to submit", failed, total)
	}
	return nil
}

// absolutize resolves each line's relative out and input paths here, as the server knows nothing of
// this directory. It keeps every line, so the server's line numbers still match; one it cannot parse
// goes through unchanged for the server to report.
func absolutize(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 8<<20)
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
