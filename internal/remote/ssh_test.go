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
func TestInterruptLeavesConnectionAvailableForNextCommand(t *testing.T) {
	addr, key := testSSH(t)
	c := fixtureClient(t, addr, key)
	if e := c.Prepare(context.Background()); e != nil {
		t.Fatal(e)
	}
	source, _ := c.Command("printf started; exec sleep 30", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, e := c.Execute(ctx, source, cancelOnOutput{cancel}, io.Discard); !errors.Is(e, context.Canceled) {
		t.Fatal("interrupt did not stop command", e)
	}
	if output, e := c.capture(context.Background(), "printf available"); e != nil || output != "available" {
		t.Fatal("connection unavailable for next command", output, e)
	}
}

func TestWaitDetectsAssetSessionEndWhileGatewayStaysAlive(t *testing.T) {
	addr, key := testSSH(t)
	c := fixtureClient(t, addr, key)
	if e := c.hold(context.Background(), "printf 'ready\\n'; sleep 0.05", "hold failed"); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("asset session exit not detected")
	}
	if _, e := c.capture(context.Background(), "printf gateway-alive"); e != nil {
		t.Fatal("test lost gateway transport", e)
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
func TestSameAccountConnectionsRunIndependently(t *testing.T) {
	addr, key := testSSH(t)
	first := fixtureClient(t, addr, key)
	second := fixtureClient(t, addr, key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []*Client{first, second} {
		if e := c.Prepare(ctx); e != nil {
			t.Fatal(e)
		}
	}
	output, writer := io.Pipe()
	defer output.Close()
	defer writer.Close()
	running, stop := context.WithCancel(ctx)
	defer stop()
	finished := make(chan error, 1)
	source, e := first.Command("printf started; exec sleep 30", "")
	if e != nil {
		t.Fatal(e)
	}
	go func() {
		_, e := first.Execute(running, source, writer, io.Discard)
		writer.CloseWithError(e)
		finished <- e
	}()
	started := make([]byte, len("started"))
	if _, e = io.ReadFull(output, started); e != nil || string(started) != "started" {
		t.Fatal("first command did not start", e)
	}
	source, e = second.Command("printf parallel", "")
	if e != nil {
		t.Fatal(e)
	}
	var result bytes.Buffer
	if code, e := second.Execute(ctx, source, &result, io.Discard); e != nil || code != 0 || result.String() != "parallel" {
		t.Fatal("second connection could not execute concurrently", result.String(), code, e)
	}
	stop()
	if e = <-finished; !errors.Is(e, context.Canceled) {
		t.Fatal("first command did not stop", e)
	}
	first.Close()
	if result, e := second.capture(ctx, "printf available"); e != nil || result != "available" {
		t.Fatal("closing first connection affected second", result, e)
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
