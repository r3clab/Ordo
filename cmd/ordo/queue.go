package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/r3clab/Ordo/internal/server"
)

const queueUsage = "Usage: ordo queue <list|get|cancel|stop> [--state DIR] [--id ID | --out DIR | --under DIR] [--reason TEXT]\n\nlist shows every job, or those whose out lies under --under. get shows one job: by ID, or the latest\nthat writes --out. cancel and stop act on one job, or on every unfinished job under --under. stop ends\nthe running step early and keeps its result, classified by exit status, so later steps still run;\ncancel abandons the job, which then delivers nothing."

type selector struct{ id, out, under string }

func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		_, err := fmt.Fprintln(out, queueUsage)
		return err
	}
	operation := args[0]
	f := newFlags("queue")
	var root, reason string
	var sel selector
	stateFlag(f, &root)
	f.StringVar(&sel.id, "id", "", "job ID")
	f.StringVar(&sel.out, "out", "", "the job that writes this directory")
	f.StringVar(&sel.under, "under", "", "the jobs whose out lies under this directory")
	f.StringVar(&reason, "reason", "", "why a job is stopped or cancelled")
	if err := parse(f, args[1:], out, queueUsage); err != nil {
		return err
	}
	if root == "" || f.NArg() != 0 {
		return errors.New("--state is required")
	}
	for _, dir := range []*string{&sel.out, &sel.under} {
		if *dir != "" {
			abs, err := filepath.Abs(*dir)
			if err != nil {
				return err
			}
			*dir = abs
		}
	}
	client := server.Client(server.Socket(root))
	defer client.CloseIdleConnections()
	switch operation {
	case "list":
		if sel.id != "" || sel.out != "" {
			return errors.New("list selects jobs with --under only")
		}
		jobs, err := sel.list(ctx, client)
		if err != nil {
			return err
		}
		return printJSON(out, jobs)
	case "get":
		if sel.under != "" {
			return errors.New("get takes --id or --out")
		}
		ids, err := sel.resolve(ctx, client, false)
		if err != nil {
			return err
		}
		var job json.RawMessage
		if err := server.Call(ctx, client, http.MethodGet, "/v1/jobs/"+ids[len(ids)-1], nil, &job); err != nil {
			return err
		}
		return printJSON(out, job)
	case "cancel", "stop":
		ids, err := sel.resolve(ctx, client, true)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(out)
		for _, id := range ids {
			if err := server.Call(ctx, client, http.MethodPost, "/v1/jobs/"+id+"/"+operation, server.StopRequest{Reason: reason}, nil); err != nil {
				return err
			}
			if err := encoder.Encode(map[string]any{"id": id, "requested": operation}); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("unknown queue command")
}

func (sel selector) list(ctx context.Context, client *http.Client) ([]server.Summary, error) {
	query := url.Values{}
	if sel.out != "" {
		query.Set("out", sel.out)
	}
	if sel.under != "" {
		query.Set("under", sel.under)
	}
	return listJobs(ctx, client, query)
}

func listJobs(ctx context.Context, client *http.Client, query url.Values) ([]server.Summary, error) {
	var jobs []server.Summary
	err := server.Call(ctx, client, http.MethodGet, "/v1/jobs?"+query.Encode(), nil, &jobs)
	return jobs, err
}

// resolve finds the selected jobs' IDs, oldest first.
func (sel selector) resolve(ctx context.Context, client *http.Client, unfinished bool) ([]string, error) {
	set := 0
	for _, value := range []string{sel.id, sel.out, sel.under} {
		if value != "" {
			set++
		}
	}
	if set != 1 {
		return nil, errors.New("select jobs with one of --id, --out or --under")
	}
	if sel.id != "" {
		return []string{sel.id}, nil
	}
	jobs, err := sel.list(ctx, client)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, j := range jobs {
		if !unfinished || !j.State.Terminal() {
			ids = append(ids, j.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("no job matches")
	}
	return ids, nil
}
