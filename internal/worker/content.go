package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (a *Agent) contentRoutes(mux *http.ServeMux) {
	blobs := filepath.Join(a.cfg.StateDir, "blobs")
	mux.HandleFunc("GET /v1/blobs/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if !content.Valid(hash) {
			http.Error(w, "invalid digest", 400)
			return
		}
		http.ServeFile(w, r, filepath.Join(blobs, hash))
	})
	mux.HandleFunc("PUT /v1/blobs/{hash}", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		hash, err := content.Put(blobs, r.Body, r.PathValue("hash"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		respond(w, 200, map[string]string{"sha256": hash})
	})
	mux.HandleFunc("POST /v1/inputs/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if !content.Valid(hash) {
			http.Error(w, "invalid digest", 400)
			return
		}
		file := filepath.Join(blobs, hash)
		actual, err := content.Hash(file)
		if err == nil && actual != hash {
			err = errors.New("cached blob checksum mismatch")
		}
		if err == nil {
			err = content.Extract(file, filepath.Join(a.cfg.StateDir, "trees", hash))
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		respond(w, 200, map[string]string{"sha256": hash})
	})
	mux.HandleFunc("POST /v1/images", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		var req struct {
			Image   string `json:"image"`
			Archive string `json:"archive"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || !imageIdentity.MatchString(req.Image) || !content.Valid(req.Archive) {
			http.Error(w, "invalid image/archive identity", 400)
			return
		}
		if id, err := task.ImageID(r.Context(), req.Image); err == nil && id == req.Image {
			respond(w, 200, req)
			return
		}
		if err := task.LoadImage(r.Context(), filepath.Join(blobs, req.Archive), req.Image); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		respond(w, 200, req)
	})
	mux.HandleFunc("GET /v1/results/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		id := r.PathValue("id")
		a.mu.Lock()
		run, ok := a.attempts[id]
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if run.State != "finished" {
			http.Error(w, "results are not ready", http.StatusConflict)
			return
		}
		dir, err := os.MkdirTemp(a.cfg.StateDir, ".result-")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		for _, name := range []string{"run.json", "stdout.log", "stderr.log", "outputs.tar"} {
			source := filepath.Join(a.runDir(id), name)
			if name == "outputs.tar" && run.Result != nil && !run.Result.Request.ExportOutputs {
				continue
			}
			if err := os.Link(source, filepath.Join(dir, name)); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		archive := filepath.Join(a.cfg.StateDir, ".result-"+id+"-"+filepath.Base(dir)+".tar")
		defer func() { _ = os.Remove(archive) }()
		hash, err := content.Archive(dir, archive)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("X-Ordo-SHA256", hash)
		http.ServeFile(w, r, archive)
	})
}

// Upload streams a verified content blob; an existing immutable blob avoids retransmission.
func Upload(ctx context.Context, client *http.Client, file, digest string) error {
	if !content.Valid(digest) {
		return errors.New("invalid blob digest")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://ordo/v1/blobs/"+digest, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			return nil
		}
		if resp.StatusCode != 404 {
			return fmt.Errorf("blob check HTTP %d", resp.StatusCode)
		}
	} else {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, "http://ordo/v1/blobs/"+digest, f)
	if err != nil {
		return err
	}
	streaming := *client
	streaming.Timeout = 0
	resp, err = streaming.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload HTTP %d: %s", resp.StatusCode, data)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
func EnsureImage(ctx context.Context, client *http.Client, file, image string) error {
	hash, err := content.Hash(file)
	if err != nil {
		return err
	}
	if err := Upload(ctx, client, file, hash); err != nil {
		return err
	}
	streaming := *client
	streaming.Timeout = 0
	return Call(ctx, &streaming, http.MethodPost, "/v1/images", map[string]string{"image": image, "archive": hash}, nil)
}
func EnsureInput(ctx context.Context, client *http.Client, file, hash string) error {
	if err := Upload(ctx, client, file, hash); err != nil {
		return err
	}
	streaming := *client
	streaming.Timeout = 0
	return Call(ctx, &streaming, http.MethodPost, "/v1/inputs/"+hash, nil, nil)
}
func FetchResult(ctx context.Context, client *http.Client, id, dest string) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid attempt ID")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ordo/v1/results/"+id, nil)
	if err != nil {
		return err
	}
	streaming := *client
	streaming.Timeout = 0
	resp, err := streaming.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("result HTTP %d", resp.StatusCode)
	}
	hash := resp.Header.Get("X-Ordo-SHA256")
	if !content.Valid(hash) {
		return errors.New("result missing checksum")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".receive-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := content.Put(tmp, resp.Body, hash); err != nil {
		return err
	}
	tree := filepath.Join(tmp, "result")
	if err := content.Extract(filepath.Join(tmp, hash), tree); err != nil {
		return err
	}
	record, err := task.ReadResult(tree)
	if err != nil {
		return err
	}
	if record.Request.ExportOutputs {
		actual, err := content.Hash(filepath.Join(tree, "outputs.tar"))
		if err != nil {
			return err
		}
		if actual != record.OutputsSHA256 {
			return errors.New("output archive checksum mismatch")
		}
		if err := content.Extract(filepath.Join(tree, "outputs.tar"), filepath.Join(tree, "outputs")); err != nil {
			return err
		}
	}
	if _, err := os.Stat(dest); err == nil {
		return errors.New("result destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tree, dest); err != nil {
		return err
	}
	return content.SyncDir(filepath.Dir(dest))
}
