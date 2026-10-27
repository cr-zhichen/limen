package session

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cr-zhichen/limen/internal/config"
	"github.com/cr-zhichen/limen/internal/review"
)

type Remote interface {
	Command(string, string) (string, error)
	Execute(context.Context, string, io.Writer, io.Writer) (int, error)
}
type Reviewer interface {
	Check(context.Context, string, string) (review.Decision, error)
}
type Request struct {
	ID      string `json:"id"`
	Op      string `json:"op"`
	Command string `json:"command,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
}
type Event struct {
	Type     string           `json:"type"`
	ID       string           `json:"id,omitempty"`
	Data     string           `json:"data,omitempty"`
	Encoding string           `json:"encoding,omitempty"`
	ExitCode *int             `json:"exit_code,omitempty"`
	Error    string           `json:"error,omitempty"`
	Decision *review.Decision `json:"review,omitempty"`
}
type Output struct {
	mu      sync.Mutex
	Encoder *json.Encoder
}

func (o *Output) Send(e Event) error { o.mu.Lock(); defer o.mu.Unlock(); return o.Encoder.Encode(e) }

type stream struct {
	o        *Output
	id, kind string
	pending  []byte
}

func (s *stream) Write(b []byte) (int, error) {
	s.pending = append(s.pending, b...)
	end := 0
	for end < len(s.pending) && utf8.FullRune(s.pending[end:]) {
		_, size := utf8.DecodeRune(s.pending[end:])
		end += size
	}
	if end > 0 {
		if err := s.emit(s.pending[:end]); err != nil {
			return 0, err
		}
		s.pending = append(s.pending[:0], s.pending[end:]...)
	}
	return len(b), nil
}
func (s *stream) emit(b []byte) error {
	event := Event{Type: s.kind, ID: s.id, Data: string(b)}
	if !utf8.Valid(b) {
		event.Encoding = "base64"
		event.Data = base64.StdEncoding.EncodeToString(b)
	}
	return s.o.Send(event)
}
func (s *stream) flush() error {
	if len(s.pending) == 0 {
		return nil
	}
	return s.emit(s.pending)
}

type Runner struct {
	Remote   Remote
	Reviewer Reviewer
	Mode     string
	Settings config.Session
	Output   *Output
}

// CommandError is a completed request that failed or was denied. The persistent
// session may accept another request; transport failures terminate the session.
type CommandError struct{ Message string }

func (e *CommandError) Error() string { return e.Message }
func (r *Runner) reject(id, message string, decision *review.Decision) error {
	if err := r.Output.Send(Event{Type: "result", ID: id, Error: message, Decision: decision}); err != nil {
		return err
	}
	return &CommandError{Message: message}
}

func (r *Runner) Exec(ctx context.Context, req Request) error {
	ctx, cancel := config.WithTimeout(ctx, r.Settings.CommandTimeout)
	defer cancel()
	source, err := r.Remote.Command(req.Command, req.Cwd)
	if err != nil {
		return r.reject(req.ID, err.Error(), nil)
	}
	decision, err := r.Reviewer.Check(ctx, r.Mode, source)
	if err != nil {
		return r.reject(req.ID, err.Error(), nil)
	}
	if !decision.Allowed {
		return r.reject(req.ID, decision.Reason, &decision)
	}
	stdout := &stream{o: r.Output, id: req.ID, kind: "stdout"}
	stderr := &stream{o: r.Output, id: req.ID, kind: "stderr"}
	code, err := r.Remote.Execute(ctx, source, stdout, stderr)
	if flushErr := stdout.flush(); flushErr != nil {
		return flushErr
	}
	if flushErr := stderr.flush(); flushErr != nil {
		return flushErr
	}
	event := Event{Type: "result", ID: req.ID, ExitCode: &code, Decision: &decision}
	if err != nil {
		event.Error = err.Error()
	}
	if e := r.Output.Send(event); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return &CommandError{Message: fmt.Sprintf("远端命令退出状态为 %d", code)}
	}
	return nil
}
func (r *Runner) Finish() error {
	return r.Output.Send(Event{Type: "closed"})
}
func (r *Runner) Serve(ctx context.Context, input io.Reader) error {
	type line struct {
		body string
		err  error
	}
	lines := make(chan line)
	done := make(chan struct{})
	defer close(done)
	go func() {
		s := bufio.NewScanner(input)
		s.Buffer(make([]byte, 4096), 1<<20)
		for s.Scan() {
			select {
			case lines <- line{body: s.Text()}:
			case <-done:
				return
			}
		}
		err := s.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case lines <- line{err: err}:
		case <-done:
		}
	}()
	for {
		var timer *time.Timer
		var timeout <-chan time.Time
		if r.Settings.IdleTimeout > 0 {
			timer = time.NewTimer(time.Duration(r.Settings.IdleTimeout) * time.Second)
			timeout = timer.C
		}
		var item line
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-timeout:
			return errors.New("连接空闲超时，请重新连接")
		case item = <-lines:
			if timer != nil {
				timer.Stop()
			}
		}
		if item.err != nil {
			if errors.Is(item.err, io.EOF) {
				return errors.New("输入已断开；正常退出请发送 close 请求")
			}
			return item.err
		}
		var req Request
		dec := json.NewDecoder(strings.NewReader(item.body))
		dec.DisallowUnknownFields()
		err := dec.Decode(&req)
		var extra any
		if err == nil && !errors.Is(dec.Decode(&extra), io.EOF) {
			err = errors.New("每行只能包含一个请求")
		}
		if err != nil {
			if e := r.Output.Send(Event{Type: "result", Error: "请求格式无效，请按 limen skill 中的格式提交"}); e != nil {
				return e
			}
			continue
		}
		switch req.Op {
		case "close":
			return r.Finish()
		case "exec":
			if err = r.Exec(ctx, req); err != nil {
				var commandError *CommandError
				if !errors.As(err, &commandError) {
					return err
				}
			}
		default:
			if err = r.Output.Send(Event{Type: "result", ID: req.ID, Error: "op 只能是 exec 或 close"}); err != nil {
				return err
			}
		}
	}
}
