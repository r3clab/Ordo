package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/worker"
)

var pollInterval = 2 * time.Second

const fetchUsage = "Usage: ordo fetch --state DIR --into DIR [--id ID ...] [--wait]\n\nWrites each succeeded job's outputs, stdout.log, stderr.log and run.json to DIR/ID,\nskipping jobs whose directory already exists. Without --id it fetches every job.\n--wait polls until the selected jobs finish."

type fetchResult struct {
	ID     string       `json:"id"`
	State  server.State `json:"state"`
	Status string       `json:"status"`
	Error  string       `json:"error,omitempty"`
}

func fetchCommand(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("fetch")
	var root, into string
	var ids []string
	var wait bool
	f.StringVar(&root, "state", "", "server state directory")
	f.StringVar(&into, "into", "", "directory that receives one subdirectory per job")
	f.Func("id", "job to fetch (repeatable; default all)", func(id string) error {
		if !worker.ValidName(id) {
			return fmt.Errorf("invalid job ID %q", id)
		}
		ids = append(ids, id)
		return nil
	})
	f.BoolVar(&wait, "wait", false, "wait for the selected jobs to finish")
	if err := parse(f, args, out, fetchUsage); err != nil {
		return err
	}
	if root == "" || into == "" || f.NArg() != 0 {
		return errors.New("--state and --into are required")
	}
	if err := os.MkdirAll(into, 0o700); err != nil {
		return err
	}
	client := serverClient(root)
	defer client.CloseIdleConnections()
	jobs := map[string]server.Job{}
	poll := func() ([]server.Job, error) {
		var list []server.Job
		err := worker.Call(ctx, client, http.MethodGet, "/v1/jobs", nil, &list)
		for _, j := range list {
			jobs[j.ID] = j
		}
		return list, err
	}
	list, err := poll()
	if err != nil {
		return err
	}
	if ids == nil {
		for _, j := range list {
			ids = append(ids, j.ID)
		}
	}
	for _, id := range ids {
		if _, ok := jobs[id]; !ok {
			return fmt.Errorf("unknown job %s", id)
		}
	}
	encoder := json.NewEncoder(out)
	pending, failed := ids, 0
	for {
		var waiting []string
		for _, id := range pending {
			j := jobs[id]
			result := fetchResult{ID: id, State: j.State}
			switch {
			case j.State == server.Succeeded:
				if result.Status, err = fetchJob(ctx, client, id, filepath.Join(into, id)); err != nil {
					result.Error = err.Error()
				}
			case j.State.Terminal():
				result.Status = "unsuccessful"
			case wait:
				waiting = append(waiting, id)
				continue
			default:
				result.Status = "pending"
			}
			if result.Status != "fetched" && result.Status != "exists" {
				failed++
			}
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
		if len(waiting) == 0 {
			break
		}
		pending = waiting
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		if _, err := poll(); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d jobs were not fetched", failed, len(ids))
	}
	return nil
}

func fetchJob(ctx context.Context, client *http.Client, id, dest string) (string, error) {
	if _, err := os.Lstat(dest); err == nil {
		return "exists", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "failed", err
	}
	if err := worker.Download(ctx, client, "/v1/jobs/"+id+"/result", dest, nil); err != nil {
		return "failed", err
	}
	return "fetched", nil
}
