package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/server"
)

const runUsage = "Usage: ordo run [--state DIR] [--job JSON] IMAGE [ARG...]\n\nRuns IMAGE with ARGs as a job that delivers nothing, writing its output here as it runs, and exits\nwith the status it exits with. --job gives the job's other fields as a line of ordo submit without\nout, steps, image or args; resources default to 1 core and 1024 MiB. An interrupt cancels the job.\n\nExits 125 if run itself failed: misused, refused, unanswered, or the job ended without a status of\nits own, being cancelled, skipped, blocked, timed out, or out of retries for failures of its worker."

var (
	stderr io.Writer = os.Stderr
	// serverGrace outlasts a server restart.
	serverGrace = time.Minute
)

// runCommand leaves every status below 125 to the container, as podman run does, so its own failures
// exit 125.
func runCommand(ctx context.Context, args []string, out io.Writer) error {
	err := runJob(ctx, args, out)
	_, passed := errors.AsType[*exitError](err)
	switch {
	case err == nil || passed:
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return &exitError{exitRunFailed, err}
}

func runJob(ctx context.Context, args []string, out io.Writer) error {
	f := newFlags("run")
	var root, job string
	stateFlag(f, &root)
	f.StringVar(&job, "job", "{}", "the job's other fields, as JSON")
	if err := parse(f, args, out, runUsage); err != nil {
		return err
	}
	if root == "" {
		return errors.New("--state is required")
	}
	if f.NArg() == 0 {
		return errors.New("IMAGE is required")
	}
	line, err := runLine(job, f.Arg(0), f.Args()[1:])
	if err != nil {
		return err
	}
	client := server.Client(server.Socket(root))
	defer client.CloseIdleConnections()
	id, err := submitOne(ctx, client, line)
	if err != nil {
		return err
	}
	j, err := watch(ctx, client, id, out, stderr)
	if !j.State.Terminal() {
		abandon(ctx, client, id)
	}
	if err != nil {
		return err
	}
	return exited(j)
}

func runLine(job, image string, args []string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job), &fields); err != nil || fields == nil {
		return nil, errors.New("--job must be a JSON object")
	}
	for _, name := range []string{"out", "steps", "image", "args"} {
		if _, ok := fields[name]; ok {
			return nil, fmt.Errorf("--job must not give %s", name)
		}
	}
	fields["image"], _ = json.Marshal(image)
	if len(args) > 0 {
		fields["args"], _ = json.Marshal(args)
	}
	if _, ok := fields["resources"]; !ok {
		fields["resources"] = json.RawMessage(`{"cores":1,"memory_mib":1024}`)
	}
	return resolvePaths(fields)
}

func submitOne(ctx context.Context, client *http.Client, line []byte) (string, error) {
	var result server.BatchResult
	if err := post(ctx, client, append(line, '\n'), func(r server.BatchResult) error { result = r; return nil }); err != nil {
		return "", err
	}
	if result.Error != "" || result.ID == "" {
		return "", fmt.Errorf("refused: %s", result.Error)
	}
	return result.ID, nil
}

func abandon(ctx context.Context, client *http.Client, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = server.Call(ctx, client, http.MethodPost, jobPath(id)+"/cancel", server.StopRequest{Reason: "ordo run ended"}, nil)
}

// A timeout or a stop fails a job whatever its container exits with.
func exited(j server.Job) error {
	if j.State == server.Succeeded {
		return nil
	}
	if n := len(j.Attempts); j.State == server.Failed && n > 0 {
		if r := j.Attempts[n-1].Result; r != nil && r.ExitCode != 0 {
			return &exitError{r.ExitCode, nil}
		}
	}
	return fmt.Errorf("job %s %s: %s", j.ID, j.State, j.Error)
}

func watch(ctx context.Context, client *http.Client, id string, stdout, stderr io.Writer) (server.Job, error) {
	var j server.Job
	for next := 0; ; {
		latest, err := poll(ctx, client, id)
		if err != nil {
			return j, err
		}
		j = latest
		for ; next < len(j.Attempts); next++ {
			at := j.Attempts[next]
			if at.ContainerID == "" && at.ResultDir == "" {
				if !over(j, next) {
					break
				}
				continue
			}
			if prev := next - 1; prev >= 0 && j.Attempts[prev].Step == at.Step {
				_, _ = fmt.Fprintf(stderr, "ordo: retrying, as attempt %d %s\n", next, ended(j.Attempts[prev]))
			}
			if err := followAttempt(ctx, client, id, next, stdout, stderr); err != nil {
				return j, err
			}
		}
		if next == len(j.Attempts) && j.State.Terminal() || j.State == server.Blocked {
			return j, nil
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return j, err
		}
	}
}

func poll(ctx context.Context, client *http.Client, id string) (server.Job, error) {
	since := time.Now()
	for {
		var j server.Job
		err := server.Call(ctx, client, http.MethodGet, jobPath(id), nil, &j)
		if err == nil || !errors.Is(err, server.ErrUnavailable) || time.Since(since) > serverGrace {
			return j, err
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return j, err
		}
	}
}

func over(j server.Job, attempt int) bool {
	return attempt+1 < len(j.Attempts) || j.State.Terminal()
}

func ended(at server.Attempt) string {
	if at.Result != nil {
		return fmt.Sprintf("exited with status %d", at.Result.ExitCode)
	}
	return "ended without a status"
}

func followAttempt(ctx context.Context, client *http.Client, id string, attempt int, stdout, stderr io.Writer) error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []struct {
		name string
		w    io.Writer
	}{{"stdout", stdout}, {"stderr", stderr}} {
		wg.Go(func() { errs[i] = followLog(ctx, client, id, attempt, s.name, s.w) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func followLog(ctx context.Context, client *http.Client, id string, attempt int, stream string, w io.Writer) error {
	c := &counter{w: w}
	for {
		err := copyLog(ctx, client, id, attempt, stream, c)
		if err == nil || ctx.Err() != nil || c.err != nil || !unready(err) {
			return err
		}
		j, err := poll(ctx, client, id)
		if err != nil {
			return err
		}
		if j.Attempts[attempt].ResultDir == "" && over(j, attempt) {
			return nil
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

// The server answers a follow with 409 before the attempt has a container, and with 502 or 503 while
// its worker cannot be reached.
func unready(err error) bool {
	status, ok := errors.AsType[*server.StatusError](err)
	if !ok {
		return errors.Is(err, server.ErrUnavailable)
	}
	return status.Code == http.StatusConflict || status.Code == http.StatusBadGateway || status.Code == http.StatusServiceUnavailable
}

func copyLog(ctx context.Context, client *http.Client, id string, attempt int, stream string, c *counter) error {
	path := fmt.Sprintf("%s/logs/%s?attempt=%d&follow=1&offset=%d", jobPath(id), stream, attempt, c.n)
	resp, err := server.Send(ctx, client, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(c, resp.Body); c.err == nil && err != nil {
		return server.Unanswered(err)
	}
	return c.err
}

type counter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if err != nil {
		c.err = err
	}
	return n, err
}
