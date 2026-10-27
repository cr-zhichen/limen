package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testSSH(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture runs a POSIX remote shell")
	}
	if _, e := exec.LookPath("flock"); e != nil {
		t.Skip("Linux SSH fixture requires flock; exercised by Linux CI")
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	server := &ssh.ServerConfig{NoClientAuth: true}
	server.AddHostKey(signer)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); listener.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				s, chans, reqs, e := ssh.NewServerConn(conn, server)
				if e != nil {
					conn.Close()
					return
				}
				defer s.Close()
				go ssh.DiscardRequests(reqs)
				for next := range chans {
					if next.ChannelType() != "session" {
						next.Reject(ssh.UnknownChannelType, "sessions only")
						continue
					}
					ch, requests, e := next.Accept()
					if e != nil {
						return
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer ch.Close()
						for req := range requests {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							ssh.Unmarshal(req.Payload, &payload)
							req.Reply(true, nil)
							command := exec.CommandContext(ctx, "bash", "-c", payload.Command)
							command.Stdout = ch
							command.Stderr = ch.Stderr()
							input, _ := command.StdinPipe()
							go func() { _, _ = io.Copy(input, ch); _ = input.Close() }()
							if e := command.Start(); e != nil {
								return
							}
							finished := make(chan error, 1)
							go func() { finished <- command.Wait() }()
							for {
								select {
								case err := <-finished:
									code := uint32(0)
									if err != nil {
										code = 1
									}
									ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
									return
								case signal, ok := <-requests:
									if !ok {
										command.Process.Kill()
										<-finished
										return
									}
									if signal.Type == "signal" {
										command.Process.Kill()
										signal.Reply(true, nil)
									} else {
										signal.Reply(false, nil)
									}
								}
							}
						}
					}()
				}
			}()
		}
	}()
	return listener.Addr().String(), signer.PublicKey()
}

type cancelOnOutput struct{ cancel context.CancelFunc }

func (w cancelOnOutput) Write(b []byte) (int, error) { w.cancel(); return len(b), nil }
func TestInterruptLeavesConnectionAvailableForCleanup(t *testing.T) {
	addr, key := testSSH(t)
	c := fixtureClient(t, addr, key)
	dir := filepath.Join(t.TempDir(), "scratch")
	if e := c.Prepare(context.Background(), dir); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "result"), []byte("data"), 0600)
	source, _ := c.Command("printf started; exec sleep 30", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, e := c.Execute(ctx, source, cancelOnOutput{cancel}, io.Discard); !errors.Is(e, context.Canceled) {
		t.Fatal("interrupt did not stop command", e)
	}
	if e := c.Cleanup(context.Background()); e != nil {
		t.Fatal("connection unavailable for normal cleanup", e)
	}
	if _, e := os.Stat(filepath.Join(dir, "result")); !os.IsNotExist(e) {
		t.Fatal("normal interruption did not clean files")
	}
}
func fixtureClient(t *testing.T, addr string, key ssh.PublicKey) *Client {
	t.Helper()
	known := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(known, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)+"\n"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, e := dial(ctx, addr, &ssh.ClientConfig{User: "test", HostKeyCallback: HostKeyCallback(known)})
	if e != nil {
		t.Fatal(e)
	}
	c := &Client{SSH: s}
	t.Cleanup(func() { c.Close() })
	return c
}
func TestPersistentSSHRetainsOnDisconnectAndCleansOnClose(t *testing.T) {
	addr, key := testSSH(t)
	c := fixtureClient(t, addr, key)
	dir := filepath.Join(t.TempDir(), "scratch")
	ctx := context.Background()
	if e := c.Prepare(ctx, dir); e != nil {
		t.Fatal(e)
	}
	source, e := c.Command(`printf '%s' data > "$LIMEN_WRITABLE_DIR/result"`, "")
	if e != nil {
		t.Fatal(e)
	}
	if code, e := c.Execute(ctx, source, io.Discard, io.Discard); e != nil || code != 0 {
		t.Fatal(code, e)
	}
	// Clearing every writable file must not remove Limen's ownership metadata.
	source, _ = c.Command(`find "$LIMEN_WRITABLE_DIR" -mindepth 1 -delete; printf '%s' data > "$LIMEN_WRITABLE_DIR/result"`, "")
	if code, e := c.Execute(ctx, source, io.Discard, io.Discard); e != nil || code != 0 {
		t.Fatal(code, e)
	}
	c.Close()
	if b, e := os.ReadFile(filepath.Join(dir, "result")); e != nil || string(b) != "data" {
		t.Fatal("disconnect removed result", e)
	}
	c = fixtureClient(t, addr, key)
	if e = c.Prepare(ctx, dir); e != nil {
		t.Fatal(e)
	}
	source, _ = c.Command(`printf '%s' "$LIMEN_WRITABLE_DIR"`, "")
	var output bytes.Buffer
	if _, e = c.Execute(ctx, source, &output, io.Discard); e != nil || strings.TrimSpace(output.String()) != c.Writable {
		t.Fatal("reconnect did not reuse path", e)
	}
	if e = c.Cleanup(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dir, "result")); !os.IsNotExist(e) {
		t.Fatal("normal close did not clean")
	}
}

func TestConcurrentConnectionsCannotCleanEachOthersFiles(t *testing.T) {
	addr, key := testSSH(t)
	first := fixtureClient(t, addr, key)
	dir := filepath.Join(t.TempDir(), "scratch")
	ctx := context.Background()
	if e := first.Prepare(ctx, dir); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "data"), []byte("keep"), 0600)
	second := fixtureClient(t, addr, key)
	if e := second.Prepare(ctx, dir); e == nil {
		t.Fatal("second session shared active writable directory")
	}
	second.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "data")); string(b) != "keep" {
		t.Fatal("second connection deleted active data")
	}
	if e := first.Cleanup(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestScratchLinksAndExistingData(t *testing.T) {
	addr, key := testSSH(t)
	c := fixtureClient(t, addr, key)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "scratch")
	if e := c.Prepare(ctx, dir); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("keep"), 0600)
	os.Symlink(outside, filepath.Join(dir, "link"))
	if c.CheckScratch(ctx) == nil {
		t.Fatal("link allowed")
	}
	if e := c.Cleanup(ctx); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatal("cleanup followed symlink")
	}
	unowned := t.TempDir()
	os.WriteFile(filepath.Join(unowned, "original"), []byte("keep"), 0600)
	if c.Prepare(ctx, unowned) == nil {
		t.Fatal("unowned directory accepted")
	}
}
func TestHostKeyMismatchAndCancellation(t *testing.T) {
	addr, key := testSSH(t)
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	wrong, _ := ssh.NewSignerFromKey(private)
	file := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(file, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, wrong.PublicKey())), 0600)
	conn, e := dial(context.Background(), addr, &ssh.ClientConfig{User: "test", HostKeyCallback: HostKeyCallback(file)})
	if e == nil {
		conn.Close()
		t.Fatal("wrong host key accepted")
	}
	c := fixtureClient(t, addr, key)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = c.Run(ctx, "printf should-not-run", io.Discard, io.Discard)
	if !errors.Is(e, context.Canceled) && e == nil {
		t.Fatal("cancel ignored")
	}
}
