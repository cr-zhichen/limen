package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cr-zhichen/limen/internal/config"
	"github.com/cr-zhichen/limen/internal/jumpserver"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Client struct {
	SSH          *ssh.Client
	Home         string
	session      *ssh.Session
	sessionInput io.WriteCloser
	sessionDone  chan error
}

func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func Address(c *config.JumpServer) string {
	return net.JoinHostPort(c.SSHHost, strconv.Itoa(c.SSHPort))
}
func KnownHostsPath() string { return filepath.Join(config.Dir(), "known_hosts") }
func HostKeyCallback(file string) ssh.HostKeyCallback {
	return func(host string, addr net.Addr, key ssh.PublicKey) error {
		callback, err := knownhosts.New(file)
		if err == nil {
			err = callback(host, addr, key)
		}
		if err != nil {
			return fmt.Errorf("SSH 主机密钥尚未信任或已变化（%s）；请核实后运行 limen host-key --trust <指纹>", ssh.FingerprintSHA256(key))
		}
		return nil
	}
}
func dial(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, ch, req, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}
	return ssh.NewClient(c, ch, req), nil
}
func Connect(ctx context.Context, c *config.JumpServer, t jumpserver.Token) (*Client, error) {
	client, err := dial(ctx, Address(c), &ssh.ClientConfig{User: "JMS-" + t.ID, Auth: []ssh.AuthMethod{ssh.Password(t.Value)}, HostKeyCallback: HostKeyCallback(KnownHostsPath())})
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败：%w", err)
	}
	return &Client{SSH: client}, nil
}
func Probe(ctx context.Context, c *config.JumpServer, trust string) (string, error) {
	var key ssh.PublicKey
	client, err := dial(ctx, Address(c), &ssh.ClientConfig{User: "limen-host-key", HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { key = k; return errors.New("host key collected") }})
	if client != nil {
		client.Close()
	}
	if key == nil {
		return "", fmt.Errorf("无法读取主机密钥：%w", err)
	}
	fp := ssh.FingerprintSHA256(key)
	if trust == "" {
		return fp, nil
	}
	if fp != trust {
		return fp, errors.New("指纹不匹配，未保存主机密钥")
	}
	if err = os.MkdirAll(config.Dir(), 0700); err != nil {
		return fp, err
	}
	f, err := os.OpenFile(KnownHostsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fp, err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(Address(c))}, key))
	return fp, err
}

// A deadline closes the transport; an explicit cancellation interrupts the
// command channel so a healthy connection can still run subsequent commands.
func (c *Client) Run(ctx context.Context, command string, stdout, stderr io.Writer) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	stopOpen := context.AfterFunc(ctx, func() { _ = c.SSH.Close() })
	s, err := c.SSH.NewSession()
	stopOpen()
	if err != nil {
		return -1, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_ = c.SSH.Close()
			return
		}
		_ = s.Signal(ssh.SIGTERM)
		_ = s.Close()
	})
	defer stop()
	s.Stdout = stdout
	s.Stderr = stderr
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	err = s.Run(command)
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var status *ssh.ExitError
	if errors.As(err, &status) {
		return status.ExitStatus(), nil
	}
	return 0, err
}
func (c *Client) capture(ctx context.Context, command string) (string, error) {
	var out, stderr bytes.Buffer
	code, err := c.Run(ctx, command, &out, &stderr)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("远端准备失败：%s", strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
func (c *Client) Prepare(ctx context.Context) error {
	home, err := c.capture(ctx, `printf '%s' "$HOME"`)
	if err != nil {
		return err
	}
	if !path.IsAbs(home) {
		return errors.New("远端账号没有有效的主目录")
	}
	c.Home = home
	return c.KeepOpen(ctx)
}

// JumpServer routes commands and forwarding through a live asset session.
func (c *Client) KeepOpen(ctx context.Context) error {
	return c.hold(ctx, "printf 'ready\\n'; cat >/dev/null", "远端会话建立失败，请检查 JumpServer 连接权限")
}
func (c *Client) hold(ctx context.Context, source, message string) error {
	stop := context.AfterFunc(ctx, func() { _ = c.SSH.Close() })
	defer stop()
	s, err := c.SSH.NewSession()
	if err != nil {
		return err
	}
	in, err := s.StdinPipe()
	if err != nil {
		s.Close()
		return err
	}
	out, err := s.StdoutPipe()
	if err != nil {
		s.Close()
		return err
	}
	if err = s.Start(source); err != nil {
		s.Close()
		return err
	}
	ready, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || ready != "ready\n" {
		s.Close()
		return errors.New(message)
	}
	c.session = s
	c.sessionInput = in
	done := make(chan error, 1)
	c.sessionDone = done
	go func() { done <- s.Wait(); close(done) }()
	return nil
}

// The asset's session may end while the gateway transport stays connected.
func (c *Client) Wait() error {
	if c.sessionDone == nil {
		return c.SSH.Wait()
	}
	transport := make(chan error, 1)
	go func() { transport <- c.SSH.Wait() }()
	select {
	case e := <-transport:
		return e
	case e := <-c.sessionDone:
		return e
	}
}
func (c *Client) Command(command, cwd string) (string, error) {
	if strings.TrimSpace(command) == "" || strings.ContainsRune(command, 0) {
		return "", errors.New("请提交非空且不含 NUL 的命令")
	}
	if cwd == "" {
		cwd = c.Home
	}
	if strings.HasPrefix(cwd, "~/") {
		cwd = path.Join(c.Home, cwd[2:])
	}
	if !path.IsAbs(cwd) || strings.ContainsRune(cwd, 0) {
		return "", errors.New("cwd 必须是远端绝对路径或 ~/ 开头的路径")
	}
	// This exact source goes to both the reviewer and a fresh noninteractive Bash.
	return "cd -- " + Quote(cwd) + " || exit\n" + command, nil
}
func (c *Client) Execute(ctx context.Context, source string, out, errout io.Writer) (int, error) {
	return c.Run(ctx, "bash --noprofile --norc -c "+Quote(source), out, errout)
}
func (c *Client) Close() error {
	if c.sessionInput != nil {
		_ = c.sessionInput.Close()
	}
	if c.session != nil {
		_ = c.session.Close()
	}
	return c.SSH.Close()
}
