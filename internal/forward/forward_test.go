package forward

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func echo(t *testing.T) net.Listener {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return listener
}
func TestTCPAndSOCKSForwarding(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(strconv.FormatBool(dynamic), func(t *testing.T) {
			upstream := echo(t)
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, listener, (&net.Dialer{}).DialContext, upstream.Addr().String(), dynamic, nil)
			}()
			c, e := net.Dial("tcp", listener.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			if dynamic {
				c.Write([]byte{5, 1, 0})
				response := make([]byte, 2)
				if _, e = io.ReadFull(c, response); e != nil || !bytes.Equal(response, []byte{5, 0}) {
					t.Fatal(response, e)
				}
				_, port, _ := net.SplitHostPort(upstream.Addr().String())
				n, _ := strconv.Atoi(port)
				request := []byte{5, 1, 0, 3, 9}
				request = append(request, []byte("localhost")...)
				request = binary.BigEndian.AppendUint16(request, uint16(n))
				c.Write(request)
				reply := make([]byte, 10)
				if _, e = io.ReadFull(c, reply); e != nil || reply[1] != 0 {
					t.Fatal(reply, e)
				}
			}
			payload := []byte("tunnel 中文\x00\xff")
			if _, e = c.Write(payload); e != nil {
				t.Fatal(e)
			}
			out := make([]byte, len(payload))
			if _, e = io.ReadFull(c, out); e != nil || !bytes.Equal(out, payload) {
				t.Fatal("forwarded bytes changed", e)
			}
			cancel()
			select {
			case e = <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("forward did not stop")
			}
			if c, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
				c.Close()
				t.Fatal("listener survived CLI exit")
			}
		})
	}
}
func TestForwardAddressParsing(t *testing.T) {
	spec, e := Parse("8080:[::1]:443")
	if e != nil || spec.Listen != "127.0.0.1:8080" || spec.Target != "[::1]:443" {
		t.Fatal(spec, e)
	}
	for _, s := range []string{"bad", "8080:localhost:no", "1:host:65536"} {
		if _, e = Parse(s); e == nil {
			t.Fatal("invalid forward accepted", s)
		}
	}
}
