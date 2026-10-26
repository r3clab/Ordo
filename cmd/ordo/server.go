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
)

func decodeFile(file string, value any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return worker.DecodeJSON(f, value)
}

func serverCommand(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("server", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var root, config, address string
	f.StringVar(&address, "listen", "127.0.0.1:7331", "loopback web UI address")
	f.StringVar(&root, "state", "", "private server state directory")
	f.StringVar(&config, "workers", "", "JSON array of worker connection configurations")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(out, "Usage: ordo server --state DIR --workers workers.json [--listen 127.0.0.1:7331]")
		}
		return err
	}
	if root == "" || config == "" || f.NArg() != 0 {
		return errors.New("--state and --workers are required")
	}
	var workers []worker.Remote
	if err := decodeFile(config, &workers); err != nil {
		return err
	}
	s, err := server.Open(root, workers)
	if err != nil {
		return err
	}
	err = s.Run(ctx, address)
	return errors.Join(err, s.Close())
}
func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		_, err := fmt.Fprintln(out, "Usage: ordo queue <submit|workflow|list|get|cancel|cancel-workflow> --state DIR [--file job.json | --id ID]")
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
	client := worker.Client(server.Socket(root))
	client.Timeout = 0
	defer client.CloseIdleConnections()
	var result any
	switch operation {
	case "workflow":
		var flow server.Workflow
		if err := decodeFile(file, &flow); err != nil {
			return err
		}
		if id != "" {
			flow.ID = id
		}
		if err := worker.Call(ctx, client, http.MethodPost, "/v1/workflows", flow, &result); err != nil {
			return err
		}
	case "cancel", "cancel-workflow":
		base := "/v1/jobs/"
		if operation == "cancel-workflow" {
			base = "/v1/workflows/"
		}
		if err := worker.Call(ctx, client, http.MethodPost, base+id+"/cancel", nil, &result); err != nil {
			return err
		}
	case "submit":
		var req server.Request
		if err := decodeFile(file, &req); err != nil {
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
