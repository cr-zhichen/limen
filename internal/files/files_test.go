package files

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

func client(t *testing.T, handlers ...sftp.Handlers) *sftp.Client {
	t.Helper()
	a, b := net.Pipe()
	h := sftp.InMemHandler()
	if len(handlers) > 0 {
		h = handlers[0]
	}
	server := sftp.NewRequestServer(b, h)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve() }()
	c, e := sftp.NewClientPipe(a, a)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); server.Close(); <-done })
	return c
}

type failingWriter struct{ sftp.FileWriter }
type fullDisk struct{}

func (fullDisk) WriteAt([]byte, int64) (int, error) { return 0, errors.New("disk full") }
func (f failingWriter) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	w, e := f.FileWriter.Filewrite(r)
	if e == nil && strings.Contains(r.Filepath, ".limen-upload-") {
		return fullDisk{}, nil
	}
	return w, e
}

func TestFailedUploadPreservesExistingRemoteFile(t *testing.T) {
	h := sftp.InMemHandler()
	h.FilePut = failingWriter{h.FilePut}
	c := client(t, h)
	old, e := c.Create("/destination")
	if e != nil {
		t.Fatal(e)
	}
	old.Write([]byte("original"))
	old.Close()
	source := filepath.Join(t.TempDir(), "source")
	os.WriteFile(source, []byte("replacement"), 0644)
	if e = Upload(c, source, "/destination", false); e == nil {
		t.Fatal("failed write reported success")
	}
	old, e = c.Open("/destination")
	if e != nil {
		t.Fatal(e)
	}
	defer old.Close()
	got, _ := io.ReadAll(old)
	if string(got) != "original" {
		t.Fatal("original remote file lost")
	}
}

func TestOverwriteRetainsDestinationPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions")
	}
	a, b := net.Pipe()
	server, e := sftp.NewServer(b)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { defer close(done); server.Serve() }()
	c, e := sftp.NewClientPipe(a, a)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); server.Close(); <-done })
	destination := filepath.Join(t.TempDir(), "destination")
	old, e := c.Create(destination)
	if e != nil {
		t.Fatal(e)
	}
	old.Close()
	if e = c.Chmod(destination, 0600); e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(t.TempDir(), "source")
	os.WriteFile(source, []byte("replacement"), 0644)
	if e = Upload(c, source, destination, false); e != nil {
		t.Fatal(e)
	}
	remote, e := c.Stat(destination)
	if e != nil || remote.Mode().Perm() != 0600 {
		t.Fatal("remote permissions changed", e)
	}
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("old"), 0600)
	c.Chmod(destination, 0644)
	if e = Download(c, destination, target, false); e != nil {
		t.Fatal(e)
	}
	local, _ := os.Stat(target)
	if local.Mode().Perm() != 0600 {
		t.Fatal("local permissions changed")
	}
}
func TestBinaryAndDirectoryRoundtrip(t *testing.T) {
	c := client(t)
	src := t.TempDir()
	payload := append([]byte("文件内容\x00"), bytes.Repeat([]byte{0xff, 0, 1, 2, 3}, 30000)...)
	os.Mkdir(filepath.Join(src, "nested"), 0700)
	os.WriteFile(filepath.Join(src, "nested", "result.bin"), payload, 0600)
	if e := Upload(c, src, "/upload", true); e != nil {
		t.Fatal(e)
	}
	entries, e := List(c, "/upload/nested")
	if e != nil || len(entries) != 1 || entries[0].Size != int64(len(payload)) {
		t.Fatal(entries, e)
	}
	dst := filepath.Join(t.TempDir(), "download")
	if e = Download(c, "/upload", dst, true); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(dst, "nested", "result.bin"))
	if e != nil || !bytes.Equal(got, payload) {
		t.Fatal("file content changed", e)
	}
	target := filepath.Join(dst, "nested", "result.bin")
	if e = os.WriteFile(target, []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = Download(c, "/upload/nested/result.bin", target, false); e != nil {
		t.Fatal("cannot replace existing download", e)
	}
	got, _ = os.ReadFile(target)
	if !bytes.Equal(got, payload) {
		t.Fatal("replacement content changed")
	}
}
func TestDownloadFailurePreservesExistingDestination(t *testing.T) {
	c := client(t)
	dst := filepath.Join(t.TempDir(), "file")
	os.WriteFile(dst, []byte("original"), 0600)
	if Download(c, "/missing", dst, false) == nil {
		t.Fatal("missing source succeeded")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "original" {
		t.Fatal("existing destination lost")
	}
}
func TestRecursiveDownloadDoesNotFollowLocalLink(t *testing.T) {
	c := client(t)
	c.MkdirAll("/data/nested")
	f, _ := c.Create("/data/nested/file")
	f.Write([]byte("incoming"))
	f.Close()
	dst := t.TempDir()
	outside := t.TempDir()
	original := filepath.Join(outside, "file")
	os.WriteFile(original, []byte("keep"), 0600)
	if e := os.Symlink(outside, filepath.Join(dst, "nested")); e != nil {
		t.Skip("symlink unavailable", e)
	}
	if Download(c, "/data", dst, true) == nil {
		t.Fatal("local symlink followed")
	}
	got, _ := os.ReadFile(original)
	if string(got) != "keep" {
		t.Fatal("outside destination overwritten")
	}
}
