package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/worker"
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
	var tools image.Tools
	services := map[string]string{}
	f.StringVar(&address, "listen", "127.0.0.1:7331", "loopback web UI address")
	f.StringVar(&root, "state", "", "private server state directory")
	f.StringVar(&config, "workers", "", "JSON array of worker connection configurations")
	f.Func("service", "NAME=HOST:PORT reachable from this server, published to tasks at /services/NAME/socket (repeatable)", keyValue(services, "service"))
	f.StringVar(&tools.Nix, "nix", "nix", "nix executable that builds {\"nix\": INSTALLABLE} images")
	f.StringVar(&tools.Skopeo, "skopeo", cmp.Or(os.Getenv("ORDO_SKOPEO"), "skopeo"), "nix2container skopeo that exports them (default $ORDO_SKOPEO)")
	if err := parse(f, args, out, "Usage: ordo server --state DIR --workers workers.json [--listen 127.0.0.1:7331] [--service NAME=HOST:PORT ...] [--nix PATH] [--skopeo PATH]"); err != nil {
		return err
	}
	if root == "" || config == "" || f.NArg() != 0 {
		return errors.New("--state and --workers are required")
	}
	var workers []worker.Remote
	if err := decodeFile(config, &workers); err != nil {
		return err
	}
	s, err := server.Open(root, workers, services, tools)
	if err != nil {
		return err
	}
	err = s.Run(ctx, address)
	return errors.Join(err, s.Close())
}

// The queue API is a local socket, and nix builds behind a submit can take minutes.
func serverClient(root string) *http.Client {
	client := worker.Client(server.Socket(root))
	client.Timeout = 0
	return client
}

func keyValue(values map[string]string, kind string) func(string) error {
	return func(value string) error {
		name, text, ok := strings.Cut(value, "=")
		if !ok || name == "" {
			return fmt.Errorf("%s must be NAME=VALUE", kind)
		}
		if _, exists := values[name]; exists {
			return fmt.Errorf("duplicate %s %s", kind, name)
		}
		values[name] = text
		return nil
	}
}

func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		_, err := fmt.Fprintln(out, "Usage: ordo queue <workflow|list|get|cancel|cancel-workflow|stop> --state DIR [--file workflow.json | --id ID [--reason TEXT]]\n\nstop ends a running job early and keeps its result, classified by exit status; cancel abandons it and skips its dependents.")
		return err
	}
	operation := args[0]
	f := flag.NewFlagSet("queue", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var root, file, id, reason string
	f.StringVar(&root, "state", "", "server state directory")
	f.StringVar(&file, "file", "", "JSON workflow")
	f.StringVar(&id, "id", "", "job ID")
	f.StringVar(&reason, "reason", "", "why a job is stopped or cancelled")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if root == "" || f.NArg() != 0 {
		return errors.New("--state is required")
	}
	client := serverClient(root)
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
	case "cancel", "cancel-workflow", "stop":
		endpoint := "/v1/jobs/" + id + "/" + operation
		if operation == "cancel-workflow" {
			endpoint = "/v1/workflows/" + id + "/cancel"
		}
		if err := worker.Call(ctx, client, http.MethodPost, endpoint, worker.StopRequest{Reason: reason}, &result); err != nil {
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
