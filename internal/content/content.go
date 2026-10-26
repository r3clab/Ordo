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
func Put(dir string, r io.Reader, want string) (string, error) {
	if want != "" && !Valid(want) {
		return "", errors.New("invalid content digest")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r, MaxSize+1))
	if err != nil {
		return "", err
	}
	if n > MaxSize {
		return "", errors.New("content exceeds 64 GiB limit")
	}
	id := hex.EncodeToString(hash.Sum(nil))
	if want != "" && id != want {
		return "", errors.New("content checksum mismatch")
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, id)); err != nil {
		return "", err
	}
	return id, SyncDir(dir)
}
func PutFile(dir, file, want string) (string, error) {
	if Valid(want) {
		if info, err := os.Lstat(filepath.Join(dir, want)); err == nil && info.Mode().IsRegular() {
			return want, nil
		}
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	id, err := Put(dir, f, want)
	return id, errors.Join(err, f.Close())
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
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(dir)
}
func Hash(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Archive publishes only a complete, synced tar. Only directories and regular files are supported.
func Archive(dir, dest string) (string, error) {
	hash := sha256.New()
	err := WriteAtomic(dest, func(f io.Writer) error {
		tw := tar.NewWriter(io.MultiWriter(f, hash))
		return errors.Join(writeTree(tw, dir), tw.Close())
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeTree(tw *tar.Writer, dir string) error {
	return filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
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
		name, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		header := &tar.Header{Name: filepath.ToSlash(name), Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Size: info.Size()}
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
}

func Extract(archive, dest string) error {
	if info, err := os.Lstat(dest); err == nil {
		if !info.IsDir() {
			return errors.New("snapshot destination is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".extract-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
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
		err = errors.Join(err, out.Sync(), out.Close())
		if err != nil {
			return err
		}
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
		if info, e := os.Stat(dest); e == nil && info.IsDir() {
			return nil
		}
		return err
	}
	return SyncDir(filepath.Dir(dest))
}
