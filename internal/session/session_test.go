package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/cr-zhichen/limen/internal/config"
	"github.com/cr-zhichen/limen/internal/review"
)

type fakeRemote struct {
	executed []string
}

func (f *fakeRemote) Command(command, cwd string) (string, error) { return command, nil }
func (f *fakeRemote) Execute(_ context.Context, s string, out, errout io.Writer) (int, error) {
	f.executed = append(f.executed, s)
	fmt.Fprint(out, "answer")
	return 0, nil
}

type testReviewer struct{}

func (testReviewer) Check(_ context.Context, mode, command string) (review.Decision, error) {
	if command == "reject" {
		return review.Decision{Reason: "blocked"}, nil
	}
	if command == "error" {
		return review.Decision{}, errors.New("review unavailable")
	}
	return review.Decision{Allowed: true}, nil
}
func TestSessionLifetimeAndGate(t *testing.T) {
	for _, normal := range []bool{false, true} {
		t.Run(fmt.Sprint(normal), func(t *testing.T) {
			f := &fakeRemote{}
			var output bytes.Buffer
			r := Runner{Remote: f, Reviewer: testReviewer{}, Mode: config.ReadOnly, Output: &Output{Encoder: json.NewEncoder(&output)}}
			input := "{\"op\":\"exec\",\"command\":\"reject\"}\n{\"op\":\"exec\",\"command\":\"error\"}\n{\"id\":\"ok\",\"op\":\"exec\",\"command\":\"read\"}\n"
			if normal {
				input += "{\"op\":\"close\"}\n"
			}
			e := r.Serve(context.Background(), strings.NewReader(input))
			if normal != (e == nil) {
				t.Fatal("wrong lifecycle", e)
			}
			if len(f.executed) != 1 || f.executed[0] != "read" {
				t.Fatal("unreviewed execution", f.executed)
			}
			dec := json.NewDecoder(&output)
			success := false
			closed := false
			for {
				var event Event
				if e := dec.Decode(&event); e == io.EOF {
					break
				} else if e != nil {
					t.Fatal(e)
				}
				if event.ID == "ok" && event.ExitCode != nil && *event.ExitCode == 0 {
					success = true
				}
				if event.Type == "closed" {
					closed = true
				}
			}
			if !success {
				t.Fatal("no completed result")
			}
			if closed != normal {
				t.Fatal("wrong close event", closed)
			}
		})
	}
}
func TestCancelledSessionStops(t *testing.T) {
	f := &fakeRemote{}
	r := Runner{Remote: f, Reviewer: testReviewer{}, Output: &Output{Encoder: json.NewEncoder(io.Discard)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	if !errors.Is(r.Serve(ctx, input), context.Canceled) {
		t.Fatal("session did not stop")
	}
}

func TestOutputPreservesUTF8AcrossChunksAndBinaryBytes(t *testing.T) {
	data := append([]byte("中文日志😀\n"), 0xff, 0xe4)
	var wire bytes.Buffer
	s := &stream{o: &Output{Encoder: json.NewEncoder(&wire)}, kind: "stdout"}
	for _, b := range data {
		if _, e := s.Write([]byte{b}); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.flush(); e != nil {
		t.Fatal(e)
	}
	dec := json.NewDecoder(&wire)
	var restored []byte
	for {
		var event Event
		e := dec.Decode(&event)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		b := []byte(event.Data)
		if event.Encoding == "base64" {
			b, e = base64.StdEncoding.DecodeString(event.Data)
			if e != nil {
				t.Fatal(e)
			}
		}
		restored = append(restored, b...)
	}
	if !bytes.Equal(data, restored) {
		t.Fatalf("output changed: %x != %x", data, restored)
	}
}
