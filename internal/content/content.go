// Package content stores immutable, SHA-256-addressed blobs and safe directory snapshots.
package content

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const MaxSize int64 = 64 << 30

func Valid(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 32 && id == strings.ToLower(id)
}
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func Put(dir string, r io.Reader) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	id, err := digest(io.TeeReader(r, f))
	if err != nil || Reuse(dir, id) {
		return id, err
	}
	if err := publish(f, filepath.Join(dir, id)); err != nil {
		return "", err
	}
	return id, nil
}

func digest(r io.Reader) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(r, MaxSize+1))
	if err != nil {
		return "", err
	}
	if n > MaxSize {
		return "", errors.New("content exceeds 64 GiB limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func Has(dir, id string) bool {
	if !Valid(id) {
		return false
	}
	info, err := os.Lstat(filepath.Join(dir, id))
	return err == nil && info.Mode().IsRegular()
}

// Reuse reports whether dir holds id, and if so renews its modification time, which Prune reads as
// its last use.
func Reuse(dir, id string) bool {
	if !Has(dir, id) {
		return false
	}
	now := time.Now()
	_ = os.Chtimes(filepath.Join(dir, id), now, now)
	return true
}

// Prune removes the blobs in dir that keep lacks and that were last stored or reused before cutoff.
func Prune(dir string, keep map[string]bool, cutoff time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !Valid(e.Name()) || keep[e.Name()] || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// MoveFile renames file into dir rather than copying it, so both must be on one filesystem.
func MoveFile(dir, file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	id, err := digest(f)
	if err := errors.Join(err, f.Sync(), f.Close()); err != nil || Reuse(dir, id) {
		return id, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(file, filepath.Join(dir, id)); err != nil {
		return "", err
	}
	return id, SyncDir(dir)
}

func PutTree(dir, tree string) (string, error) {
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.CloseWithError(Tar(w, tree, false))
	}()
	id, err := Put(dir, r)
	_ = r.Close()
	<-done
	return id, err
}

func WriteAtomic(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := write(f); err != nil {
		return err
	}
	return publish(f, path)
}

func publish(f *os.File, path string) error {
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// Archive publishes only a complete, synced tar that keeps modification times. Only directories and
// regular files are supported.
func Archive(dir, dest string) (string, error) {
	hash := sha256.New()
	err := WriteAtomic(dest, func(f io.Writer) error { return Tar(io.MultiWriter(f, hash), dir, true) })
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Without times every entry gets the epoch, so a snapshot's digest depends on its content alone.
func Tar(w io.Writer, dir string, times bool) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if file == dir {
			if !entry.IsDir() {
				return errors.New("snapshot source must be a directory")
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported snapshot entry: %s", file)
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		header := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Size: info.Size()}
		if times {
			// Only PAX keeps the sub-second part, which results such as test timings rely on.
			header.ModTime, header.Format = info.ModTime(), tar.FormatPAX
		}
		if info.IsDir() {
			header.Typeflag = tar.TypeDir
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		in, err := os.Open(file)
		if err != nil {
			return err
		}
		opened, err := in.Stat()
		if err != nil {
			_ = in.Close()
			return err
		}
		if !os.SameFile(info, opened) {
			_ = in.Close()
			return errors.New("input changed during snapshot")
		}
		_, copyErr := io.Copy(tw, in)
		return errors.Join(copyErr, in.Close())
	})
	return errors.Join(err, tw.Close())
}

func Unpack(r io.Reader, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".extract-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := untar(r, dir); err != nil {
		return err
	}
	// The tar reader stops at the end-of-archive marker, and a writer on a pipe still has the padding
	// after it to send.
	if _, err := io.Copy(io.Discard, io.LimitReader(r, 1<<20)); err != nil {
		return err
	}
	if err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return SyncDir(file)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if err := os.Rename(dir, dest); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(dest))
}

func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	var total int64
	// Writing an entry changes its directory's time, so directories get theirs once all are written.
	dirs := map[string]time.Time{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			for target, mtime := range dirs {
				if err := os.Chtimes(target, mtime, mtime); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || name == "." || path.Clean(name) != name || !filepath.IsLocal(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
			return errors.New("unsafe archive path")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return errors.New("archive links and special files are unsupported")
		}
		if h.Size < 0 || h.Size > MaxSize-total {
			return errors.New("expanded content exceeds limit")
		}
		total += h.Size
		target := filepath.Join(dir, filepath.FromSlash(name))
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			dirs[target] = h.ModTime
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644|fs.FileMode(h.Mode)&0o111)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if err := errors.Join(err, out.Sync(), out.Close(), os.Chtimes(target, h.ModTime, h.ModTime)); err != nil {
			return err
		}
	}
}
