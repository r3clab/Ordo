package content

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveKeepsModificationTimes(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "dir", "data")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileTime, dirTime := time.Unix(1700000000, 123456789), time.Unix(1600000000, 987654321)
	if err := os.Chtimes(file, fileTime, fileTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(src, "dir"), dirTime, dirTime); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "outputs.tar")
	if _, err := Archive(src, archive); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dest := filepath.Join(root, "dest")
	if err := Unpack(f, dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Time{"dir/data": fileTime, "dir": dirTime} {
		info, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(want) {
			t.Fatalf("%s modified at %v, want %v", name, info.ModTime(), want)
		}
	}
}

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
	id, err := Put(filepath.Join(root, "blobs"), f)
	if err != nil || id != first {
		t.Fatal(err)
	}
	tree, err := PutTree(filepath.Join(root, "blobs"), os.DirFS(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(src, "data"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if again, err := PutTree(filepath.Join(root, "blobs"), os.DirFS(src)); err != nil || again != tree {
		t.Fatal("snapshot digest depends on modification times", err)
	}
	if _, err := PutTree(filepath.Join(root, "blobs"), os.DirFS(filepath.Join(src, "data"))); err == nil {
		t.Fatal("stored a file as a tree")
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "tree")
	if err := Unpack(f, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "data"))
	if err != nil || string(data) != "hello" {
		t.Fatal(err)
	}
}
func TestArchiveKeepsSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dir", "data"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"relative": "dir/data", "absolute": "/etc/passwd", "dangling": "missing", "dir/up": ".."}
	link := func() {
		t.Helper()
		for name, target := range links {
			_ = os.Remove(filepath.Join(src, name))
			if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
				t.Fatal(err)
			}
		}
		for _, dir := range []string{src, filepath.Join(src, "dir")} {
			if err := os.Chtimes(dir, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	link()
	archive := filepath.Join(root, "outputs.tar")
	first, err := Archive(src, archive)
	if err != nil {
		t.Fatal(err)
	}
	link()
	if second, err := Archive(src, archive); err != nil || second != first {
		t.Fatal("archive digest depends on when its links were made", err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dest := filepath.Join(root, "dest")
	if err := Unpack(f, dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range links {
		if got, err := os.Readlink(filepath.Join(dest, name)); err != nil || got != want {
			t.Fatalf("%s links to %q, want %q: %v", name, got, want, err)
		}
	}
}

func TestRejectArchiveEscapes(t *testing.T) {
	root := t.TempDir()
	escape := filepath.Join(root, "escape")
	for _, headers := range [][]tar.Header{
		{{Name: "../escape", Typeflag: tar.TypeReg}},
		{{Name: "/absolute", Typeflag: tar.TypeReg}},
		{{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../escape"}},
		{{Name: "device", Typeflag: tar.TypeChar}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: ".."}, {Name: "link/escape", Typeflag: tar.TypeReg}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: root}, {Name: "link/escape", Typeflag: tar.TypeReg}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: ".."}, {Name: "link/escape", Typeflag: tar.TypeDir}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"}, {Name: "link", Typeflag: tar.TypeReg}},
	} {
		var data bytes.Buffer
		tw := tar.NewWriter(&data)
		for _, header := range headers {
			if err := tw.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := Unpack(&data, filepath.Join(root, "out")); err == nil {
			t.Fatalf("accepted %+v", headers)
		}
		if _, err := os.Lstat(escape); !os.IsNotExist(err) {
			t.Fatalf("%+v wrote outside the destination", headers)
		}
	}
}

func TestPruneKeepsUsedAndRecentBlobs(t *testing.T) {
	dir := t.TempDir()
	put := func(data string) string {
		t.Helper()
		id, err := Put(dir, bytes.NewReader([]byte(data)))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	kept, idle, reused, fresh := put("kept"), put("idle"), put("reused"), put("fresh")
	old := time.Now().Add(-2 * time.Hour)
	for _, id := range []string{kept, idle, reused} {
		if err := os.Chtimes(filepath.Join(dir, id), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if put("reused") != reused {
		t.Fatal("content changed its ID")
	}
	if err := Prune(dir, map[string]bool{kept: true}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{kept: true, idle: false, reused: true, fresh: true} {
		if Has(dir, id) != want {
			t.Fatalf("blob %s present = %v, want %v", id, !want, want)
		}
	}
}
