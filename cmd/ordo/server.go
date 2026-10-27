package main

import (
	"cmp"
	"context"
	"errors"
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
	return server.DecodeJSON(f, value)
}
func serverCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("server")
	var root, workersFile, address string
	var tools image.Tools
	services, secretFiles := map[string]string{}, map[string]string{}
	f.StringVar(&address, "listen", "127.0.0.1:7331", "loopback web UI address")
	f.StringVar(&root, "state", "", "private server state directory")
	f.StringVar(&workersFile, "workers", "", "JSON array of worker connection configurations")
	f.Func("service", "NAME=HOST:PORT reachable from this server, published to tasks at /services/NAME/socket (repeatable)", keyValue(services, "service", "NAME=HOST:PORT"))
	f.Func("secret", "NAME=FILE whose content, without one trailing newline, tasks listing NAME in secrets receive as $NAME (repeatable)", keyValue(secretFiles, "secret", "NAME=FILE"))
	f.StringVar(&tools.Nix, "nix", "nix", "nix executable that builds {\"nix\": INSTALLABLE} images")
	f.StringVar(&tools.Skopeo, "skopeo", cmp.Or(os.Getenv("ORDO_SKOPEO"), "skopeo"), "nix2container skopeo that exports them (default $ORDO_SKOPEO)")
	if err := parse(f, args, out, "Usage: ordo server --state DIR --workers workers.json [--listen 127.0.0.1:7331] [--service NAME=HOST:PORT ...] [--secret NAME=FILE ...] [--nix PATH] [--skopeo PATH]"); err != nil {
		return err
	}
	if root == "" || workersFile == "" || f.NArg() != 0 {
		return errors.New("--state and --workers are required")
	}
	var workers []worker.Config
	if err := decodeFile(workersFile, &workers); err != nil {
		return err
	}
	secrets, err := readSecrets(secretFiles)
	if err != nil {
		return err
	}
	s, err := server.Open(server.Config{Root: root, Workers: workers, Services: services, Secrets: secrets, Tools: tools})
	if err != nil {
		return err
	}
	err = s.Run(ctx, address)
	return errors.Join(err, s.Close())
}

func keyValue(values map[string]string, kind, shape string) func(string) error {
	return func(value string) error {
		name, text, ok := strings.Cut(value, "=")
		if !ok || name == "" {
			return fmt.Errorf("%s must be %s", kind, shape)
		}
		if _, exists := values[name]; exists {
			return fmt.Errorf("duplicate %s %s", kind, name)
		}
		values[name] = text
		return nil
	}
}

const queueUsage = "Usage: ordo queue <workflow|list|get|cancel|cancel-workflow|stop> --state DIR [--file workflow.json | --id ID [--reason TEXT]]\n\nstop ends a running job early and keeps its result, classified by exit status; cancel abandons it and skips its dependents."

func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		_, err := fmt.Fprintln(out, queueUsage)
		return err
	}
	operation := args[0]
	f := newFlags("queue")
	var root, file, id, reason string
	f.StringVar(&root, "state", "", "server state directory")
	f.StringVar(&file, "file", "", "JSON workflow")
	f.StringVar(&id, "id", "", "job ID")
	f.StringVar(&reason, "reason", "", "why a job is stopped or cancelled")
	if err := parse(f, args[1:], out, queueUsage); err != nil {
		return err
	}
	if root == "" || f.NArg() != 0 {
		return errors.New("--state is required")
	}
	method, endpoint := http.MethodPost, "/v1/jobs/"+id+"/"+operation
	var body any
	switch operation {
	case "workflow":
		var flow server.Workflow
		if err := decodeFile(file, &flow); err != nil {
			return err
		}
		if id != "" {
			flow.ID = id
		}
		endpoint, body = "/v1/workflows", flow
	case "cancel", "stop":
		body = server.StopRequest{Reason: reason}
	case "cancel-workflow":
		endpoint, body = "/v1/workflows/"+id+"/cancel", server.StopRequest{Reason: reason}
	case "list":
		method, endpoint = http.MethodGet, "/v1/jobs"
	case "get":
		method, endpoint = http.MethodGet, "/v1/jobs/"+id
	default:
		return errors.New("unknown queue command")
	}
	client := server.Client(server.Socket(root))
	defer client.CloseIdleConnections()
	var result any
	if err := server.Call(ctx, client, method, endpoint, body, &result); err != nil {
		return err
	}
	return printJSON(out, result)
}

// Editors and echo end a file with a newline that is not part of the secret.
func readSecrets(files map[string]string) (map[string]string, error) {
	values := map[string]string{}
	for name, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		values[name] = strings.TrimSuffix(string(data), "\n")
	}
	return values, nil
}
