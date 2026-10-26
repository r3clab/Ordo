package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/worker"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func serverCommand(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("server", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var root, config string
	f.StringVar(&root, "state", "", "private server state directory")
	f.StringVar(&config, "workers", "", "JSON array of worker connection configurations")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(out, "Usage: ordo server --state DIR --workers workers.json")
			return err
		}
		return err
	}
	if root == "" || config == "" || f.NArg() != 0 {
		return errors.New("--state and --workers are required")
	}
	data, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	var workers []worker.Remote
	if err := json.Unmarshal(data, &workers); err != nil {
		return err
	}
	s, err := server.Open(root, workers)
	if err != nil {
		return err
	}
	err = s.Run(ctx)
	return errors.Join(err, s.Close())
}
func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		_, err := fmt.Fprintln(out, "Usage: ordo queue <submit|list|get> --state DIR [--file job.json | --id ID]")
		return err
	}
	operation := args[0]
	f := flag.NewFlagSet("queue", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var root, file, id string
	f.StringVar(&root, "state", "", "server state directory")
	f.StringVar(&file, "file", "", "JSON request")
	f.StringVar(&id, "id", "", "job ID")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if root == "" || f.NArg() != 0 {
		return errors.New("--state is required")
	}
	client := worker.Client(filepath.Join(root, "server.sock"))
	client.Timeout = 0
	defer client.CloseIdleConnections()
	var result any
	switch operation {
	case "submit":
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var req server.Request
		if err := json.Unmarshal(data, &req); err != nil {
			return err
		}
		if id != "" {
			req.ID = id
		}
		if err := worker.Call(ctx, client, http.MethodPost, "/v1/jobs", req, &result); err != nil {
			return err
		}
	case "list":
		if err := worker.Call(ctx, client, http.MethodGet, "/v1/jobs", nil, &result); err != nil {
			return err
		}
	case "get":
		if err := worker.Call(ctx, client, http.MethodGet, "/v1/jobs/"+id, nil, &result); err != nil {
			return err
		}
	default:
		return errors.New("unknown queue command")
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
