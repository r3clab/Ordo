package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

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
	Port      uint16            `json:"port"`
	Workers   []worker.Config   `json:"workers"`
	Services  map[string]string `json:"services,omitempty"`
	SSHConfig string            `json:"ssh_config,omitempty"`
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
	if file := os.Getenv("ORDO_CONFIG"); file != "" {
		return file
	}
	if dir := xdgDir("XDG_CONFIG_HOME", ".config"); dir != "" {
		return filepath.Join(dir, "ordo", "server.json")
	}
	return ""
}

func stateFlag(f *flag.FlagSet, root *string) {
	f.StringVar(root, "state", defaultState(), "server state directory (default $ORDO_STATE, else $XDG_STATE_HOME/ordo/server)")
}

func loadServerConfig(file string) (serverConfig, error) {
	c := serverConfig{Port: 7331}
	if err := decodeFile(file, &c); err != nil {
		return c, fmt.Errorf("server config: %w", err)
	}
	if c.SSHConfig != "" && !filepath.IsAbs(c.SSHConfig) {
		c.SSHConfig = filepath.Join(filepath.Dir(file), c.SSHConfig)
	}
	return c, nil
}

const serverUsage = "Usage: ordo server [--config FILE] [--state DIR]\n\nThe config (default $ORDO_CONFIG, else $XDG_CONFIG_HOME/ordo/server.json) is a JSON object: workers (required), port, services, ssh_config. Secrets are files in the secrets directory beside it. SIGHUP reloads the config; a new port takes effect on restart."

func serverCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("server")
	var root string
	stateFlag(f, &root)
	file := f.String("config", defaultConfig(), "server configuration file (default $ORDO_CONFIG, else $XDG_CONFIG_HOME/ordo/server.json)")
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
	s, err := server.Open(server.Config{Root: root, Workers: c.Workers, Services: c.Services, SecretsDir: filepath.Join(filepath.Dir(*file), "secrets"), SSHConfig: c.SSHConfig, Tools: image.Tools{Skopeo: defaultSkopeo}})
	if err != nil {
		return err
	}
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { reloadOnHangup(ctx, hangups, s, *file, c.Port) })
	err = s.Run(ctx, c.Port)
	cancel()
	wg.Wait()
	return errors.Join(err, s.Close())
}

func reloadOnHangup(ctx context.Context, hangups <-chan os.Signal, s *server.Server, file string, port uint16) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hangups:
		}
		c, err := loadServerConfig(file)
		if err == nil {
			err = s.Reload(server.Config{Workers: c.Workers, Services: c.Services, SSHConfig: c.SSHConfig})
		}
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "reload: %v\n", err)
		case c.Port != port:
			fmt.Fprintf(os.Stderr, "reloaded; port %d takes effect on restart\n", c.Port)
		default:
			fmt.Fprintln(os.Stderr, "reloaded")
		}
	}
}
