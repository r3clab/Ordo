package content

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "input.tar")
	first, err := Archive(src, archive)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Archive(src, archive)
	if err != nil || first != second {
		t.Fatal("nondeterministic snapshot", err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	id, err := Put(filepath.Join(root, "blobs"), f, first)
	if err != nil || id != first {
		t.Fatal(err)
	}
	if id, err := PutTree(filepath.Join(root, "blobs"), src); err != nil || id != first {
		t.Fatal("tree blob differs from its archive", err)
	}
	if _, err := PutTree(filepath.Join(root, "blobs"), filepath.Join(src, "data")); err == nil {
		t.Fatal("stored a file as a tree")
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "tree")
	if err := Unpack(f, dest, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "data"))
	if err != nil || string(data) != "hello" {
		t.Fatal(err)
	}
	if _, err := Put(filepath.Join(root, "blobs"), bytes.NewBufferString("bad"), id); err == nil {
		t.Fatal("accepted wrong checksum")
	}
}
func TestRejectArchiveEscapes(t *testing.T) {
	for _, header := range []tar.Header{{Name: "../escape", Typeflag: tar.TypeReg}, {Name: "/absolute", Typeflag: tar.TypeReg}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "hard", Typeflag: tar.TypeLink, Linkname: "../escape"}, {Name: "device", Typeflag: tar.TypeChar}} {
		root := t.TempDir()
		var data bytes.Buffer
		tw := tar.NewWriter(&data)
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := Unpack(&data, filepath.Join(root, "out"), nil); err == nil {
			t.Fatalf("accepted %+v", header)
		}
	}
}
