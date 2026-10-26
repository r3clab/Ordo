package worker

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const ChecksumHeader = "X-Ordo-SHA256"

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
	mux.HandleFunc("PUT /v1/blobs/{hash}", unbounded(func(w http.ResponseWriter, r *http.Request) {
		hash, err := content.Put(blobs, r.Body, r.PathValue("hash"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		WriteJSON(w, 200, map[string]string{"sha256": hash})
	}))
	mux.HandleFunc("POST /v1/inputs/{hash}", unbounded(func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if !content.Valid(hash) {
			http.Error(w, "invalid digest", 400)
			return
		}
		file := filepath.Join(blobs, hash)
		tree := a.treeDir(hash)
		var err error
		if _, statErr := os.Lstat(tree); statErr != nil {
			var actual string
			actual, err = content.Hash(file)
			if err == nil && actual != hash {
				err = errors.New("cached blob checksum mismatch")
			}
		}
		if err == nil {
			err = content.Extract(file, tree)
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		WriteJSON(w, 200, map[string]string{"sha256": hash})
	}))
	mux.HandleFunc("POST /v1/images", unbounded(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Image   string `json:"image"`
			Archive string `json:"archive"`
		}
		if err := DecodeJSON(http.MaxBytesReader(w, r.Body, 4096), &req); err != nil || !task.ValidImageID(req.Image) || !content.Valid(req.Archive) {
			http.Error(w, "invalid image/archive identity", 400)
			return
		}
		if id, err := task.ImageID(r.Context(), req.Image); err == nil && id == req.Image {
			WriteJSON(w, 200, req)
			return
		}
		if err := task.LoadImage(r.Context(), filepath.Join(blobs, req.Archive), req.Image); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		WriteJSON(w, 200, req)
	}))
	mux.HandleFunc("GET /v1/results/{id}", unbounded(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a.mu.Lock()
		run, ok := a.attempts[id]
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if !run.Collectable() {
			http.Error(w, "results are not ready", http.StatusConflict)
			return
		}
		var parts []content.Part
		for _, name := range run.Result.Results() {
			parts = append(parts, content.Part{Path: filepath.Join(a.runDir(id), name), Name: name})
		}
		ServeTar(w, parts...)
	}))
}

// unbounded lifts the server's deadlines from a route that moves or loads whole images and archives.
func unbounded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := http.NewResponseController(w)
		_ = c.SetReadDeadline(time.Time{})
		_ = c.SetWriteDeadline(time.Time{})
		h(w, r)
	}
}

// ServeTar streams parts with their digest in a trailer, so the archive needs no staging copy.
func ServeTar(w http.ResponseWriter, parts ...content.Part) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Trailer", ChecksumHeader)
	hash := sha256.New()
	body := &sentWriter{Writer: w}
	if err := content.Tar(io.MultiWriter(body, hash), parts...); err != nil {
		if body.sent {
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Trailer")
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set(ChecksumHeader, hex.EncodeToString(hash.Sum(nil)))
}

type sentWriter struct {
	io.Writer
	sent bool
}

func (w *sentWriter) Write(p []byte) (int, error) {
	w.sent = w.sent || len(p) > 0
	return w.Writer.Write(p)
}

func uploadBlob(ctx context.Context, client *http.Client, file, digest string) error {
	if !content.Valid(digest) {
		return errors.New("invalid blob digest")
	}
	endpoint := "/v1/blobs/" + digest
	resp, err := Send(ctx, client, http.MethodHead, endpoint, "", nil)
	if err == nil {
		return resp.Body.Close()
	}
	var missing *HTTPError
	if !errors.As(err, &missing) || missing.Status != http.StatusNotFound {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	resp, err = Send(ctx, untimed(client), http.MethodPut, endpoint, "application/octet-stream", f)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
func EnsureImage(ctx context.Context, client *http.Client, file, digest, image string) error {
	if err := uploadBlob(ctx, client, file, digest); err != nil {
		return err
	}
	return Call(ctx, untimed(client), http.MethodPost, "/v1/images", map[string]string{"image": image, "archive": digest}, nil)
}
func EnsureInput(ctx context.Context, client *http.Client, file, hash string) error {
	if err := uploadBlob(ctx, client, file, hash); err != nil {
		return err
	}
	return Call(ctx, untimed(client), http.MethodPost, "/v1/inputs/"+hash, nil, nil)
}
func FetchResult(ctx context.Context, client *http.Client, id, dest string) error {
	if !ValidName(id) {
		return errors.New("invalid attempt ID")
	}
	return Download(ctx, client, "/v1/results/"+id, dest, verifyResult)
}

func verifyResult(tree string) error {
	record, err := task.ReadResult(tree)
	if err != nil {
		return err
	}
	if !record.Request.ExportOutputs {
		return nil
	}
	f, err := os.Open(filepath.Join(tree, task.OutputsArchive))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return content.Unpack(f, filepath.Join(tree, task.OutputsDir), func(_, digest string) error {
		if digest != record.OutputsSHA256 {
			return errors.New("output archive checksum mismatch")
		}
		return nil
	})
}

func Download(ctx context.Context, client *http.Client, endpoint, dest string, verify func(tree string) error) error {
	resp, err := Send(ctx, untimed(client), http.MethodGet, endpoint, "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := os.Lstat(dest); err == nil {
		return errors.New("result destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return content.Unpack(resp.Body, dest, func(tree, digest string) error {
		if want := cmp.Or(resp.Header.Get(ChecksumHeader), resp.Trailer.Get(ChecksumHeader)); !content.Valid(want) {
			return errors.New("result missing checksum")
		} else if want != digest {
			return errors.New("content checksum mismatch")
		}
		if verify != nil {
			return verify(tree)
		}
		return nil
	})
}
