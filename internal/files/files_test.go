package files

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

func client(t *testing.T) *sftp.Client {
	t.Helper()
	a, b := net.Pipe()
	server := sftp.NewRequestServer(b, sftp.InMemHandler())
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve() }()
	c, e := sftp.NewClientPipe(a, a)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); server.Close(); <-done })
	return c
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
