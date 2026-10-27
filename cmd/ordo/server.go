package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// Nix builds set this to nix2container's skopeo; stock skopeo lacks the nix: transport.
var defaultSkopeo = "skopeo"

type serverConfig struct {
	Listen   string            `json:"listen,omitempty"`
	Workers  []worker.Config   `json:"workers"`
	Services map[string]string `json:"services,omitempty"`
	Secrets  map[string]string `json:"secrets,omitempty"`
	Nix      string            `json:"nix,omitempty"`
	Skopeo   string            `json:"skopeo,omitempty"`
}

func xdgDir(env, fallback string) string {
	if dir := os.Getenv(env); filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, fallback)
}

func defaultState() string {
	if dir := os.Getenv("ORDO_STATE"); dir != "" {
		return dir
	}
	if dir := xdgDir("XDG_STATE_HOME", ".local/state"); dir != "" {
		return filepath.Join(dir, "ordo", "server")
	}
	return ""
}

func defaultConfig() string {
	if dir := xdgDir("XDG_CONFIG_HOME", ".config"); dir != "" {
		return filepath.Join(dir, "ordo", "server.json")
	}
	return ""
}

func stateFlag(f *flag.FlagSet, root *string) {
	f.StringVar(root, "state", defaultState(), "server state directory (default $ORDO_STATE, else $XDG_STATE_HOME/ordo/server)")
}

func loadServerConfig(file string) (serverConfig, error) {
	c := serverConfig{Listen: "127.0.0.1:7331", Nix: "nix", Skopeo: defaultSkopeo}
	if err := decodeFile(file, &c); err != nil {
		return c, fmt.Errorf("server config: %w", err)
	}
	for name, secret := range c.Secrets {
		if !filepath.IsAbs(secret) {
			c.Secrets[name] = filepath.Join(filepath.Dir(file), secret)
		}
	}
	return c, nil
}

const serverUsage = "Usage: ordo server [--config FILE] [--state DIR]\n\nThe config (default $XDG_CONFIG_HOME/ordo/server.json) is a JSON object: workers (required), listen, services, secrets, nix, skopeo."

func serverCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("server")
	var root string
	stateFlag(f, &root)
	file := f.String("config", defaultConfig(), "server configuration file")
	if err := parse(f, args, out, serverUsage); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("ordo server takes no arguments")
	}
	if root == "" || *file == "" {
		return errors.New("--state and --config are required without a home directory")
	}
	c, err := loadServerConfig(*file)
	if err != nil {
		return err
	}
	secrets, err := readSecrets(c.Secrets)
	if err != nil {
		return err
	}
	s, err := server.Open(server.Config{Root: root, Workers: c.Workers, Services: c.Services, Secrets: secrets, Tools: image.Tools{Nix: c.Nix, Skopeo: c.Skopeo}})
	if err != nil {
		return err
	}
	err = s.Run(ctx, c.Listen)
	return errors.Join(err, s.Close())
}

const queueUsage = "Usage: ordo queue <list|get|cancel|cancel-workflow|stop> [--state DIR] [--id ID [--reason TEXT]]\n\nstop ends a running job early and keeps its result, classified by exit status; cancel abandons it and skips its dependents."

func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		_, err := fmt.Fprintln(out, queueUsage)
		return err
	}
	operation := args[0]
	f := newFlags("queue")
	var root, id, reason string
	stateFlag(f, &root)
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
