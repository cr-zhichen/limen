package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	ReadOnly     = "read-only"
	ReadWrite    = "read-write"
	Unrestricted = "unrestricted"
)

type Secret struct {
	Value   string   `toml:"value"`
	Env     string   `toml:"env"`
	Command []string `toml:"command"`
	mu      sync.Mutex
	cached  string
}

func (s *Secret) Validate() error {
	n := 0
	if s.Value != "" {
		n++
	}
	if s.Env != "" {
		n++
	}
	if len(s.Command) > 0 {
		n++
	}
	if n != 1 {
		return errors.New("凭据必须选择 value、env、command 中的一种来源")
	}
	return nil
}

func (s *Secret) Resolve(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != "" {
		return s.cached, nil
	}
	if err := s.Validate(); err != nil {
		return "", err
	}
	value := s.Value
	if s.Env != "" {
		value = os.Getenv(s.Env)
	}
	if len(s.Command) > 0 {
		out, err := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...).Output()
		if err != nil {
			return "", errors.New("凭据命令执行失败，请检查命令及其授权状态")
		}
		value = strings.TrimRight(string(out), "\r\n")
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("凭据为空，请检查配置的来源")
	}
	s.cached = value
	return value, nil
}

type JumpServer struct {
	URL             string `toml:"url"`
	SSHHost         string `toml:"ssh_host"`
	SSHPort         int    `toml:"ssh_port"`
	AccessKeyID     Secret `toml:"access_key_id"`
	AccessKeySecret Secret `toml:"access_key_secret"`
}
type Permissions struct {
	Allowed     []string `toml:"allowed"`
	Default     string   `toml:"default"`
	WritableDir string   `toml:"read_only_writable_dir"`
}
type Session struct {
	ConnectTimeout int64 `toml:"connect_timeout"`
	IdleTimeout    int64 `toml:"idle_timeout"`
	CommandTimeout int64 `toml:"command_timeout"`
}
type Model struct {
	APIFormat       string `toml:"api_format"`
	Endpoint        string `toml:"endpoint"`
	Model           string `toml:"model"`
	APIKey          Secret `toml:"api_key"`
	ReasoningEffort string `toml:"reasoning_effort"`
}
type Config struct {
	JumpServer  JumpServer  `toml:"jumpserver"`
	Permissions Permissions `toml:"permissions"`
	Session     Session     `toml:"session"`
	Review      struct {
		Mode string `toml:"mode"`
	} `toml:"review"`
	Models struct {
		LLM Model `toml:"llm"`
		Jev Model `toml:"jev"`
	} `toml:"models"`
}

func Dir() string         { h, _ := os.UserHomeDir(); return filepath.Join(h, ".config", "limen") }
func DefaultPath() string { return filepath.Join(Dir(), "config.toml") }
func Defaults() *Config {
	c := &Config{Permissions: Permissions{Allowed: []string{ReadOnly}, Default: ReadOnly, WritableDir: "~/.local/share/limen/tmp"}, Session: Session{15, 600, 300}}
	c.JumpServer.SSHPort = 2222
	return c
}
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取配置，请先运行 limen config init：%w", err)
	}
	c := Defaults()
	if err = toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(c); err != nil {
		return nil, errors.New("配置格式无效或含有未知配置项，请对照 limen config example 修改")
	}
	return c, c.Validate()
}
func endpoint(s string) bool {
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (c *Config) Validate() error {
	j := &c.JumpServer
	if !endpoint(j.URL) || j.SSHHost == "" || j.SSHPort < 1 || j.SSHPort > 65535 {
		return errors.New("请填写有效的 jumpserver.url、ssh_host 和 ssh_port")
	}
	if e := j.AccessKeyID.Validate(); e != nil {
		return fmt.Errorf("access_key_id：%w", e)
	}
	if e := j.AccessKeySecret.Validate(); e != nil {
		return fmt.Errorf("access_key_secret：%w", e)
	}
	for _, v := range []int64{c.Session.ConnectTimeout, c.Session.IdleTimeout, c.Session.CommandTimeout} {
		if v < 0 || v > int64((1<<63-1)/time.Second) {
			return errors.New("超时必须是非负整数秒，0 表示取消超时")
		}
	}
	if !slices.Contains(c.Permissions.Allowed, c.Permissions.Default) {
		return errors.New("permissions.default 必须包含在 allowed 中")
	}
	audited := false
	for _, p := range c.Permissions.Allowed {
		switch p {
		case ReadOnly, ReadWrite:
			audited = true
		case Unrestricted:
		default:
			return errors.New("权限只能是 read-only、read-write、unrestricted")
		}
	}
	p := c.Permissions.WritableDir
	if !(strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/")) || strings.ContainsAny(p, "\x00\r\n") || p == "/" || p == "~/" {
		return errors.New("可写目录必须是专用目录的绝对路径或 ~/ 开头的路径")
	}
	if !audited && c.Review.Mode == "" {
		return nil
	}
	check := func(m *Model) error {
		if !endpoint(m.Endpoint) || m.Model == "" {
			return errors.New("所选审核模型必须填写 endpoint 和 model")
		}
		return m.APIKey.Validate()
	}
	switch c.Review.Mode {
	case "llm":
		return check(&c.Models.LLM)
	case "jev":
		if c.Models.Jev.APIFormat != "typesafe" && c.Models.Jev.APIFormat != "openrouter" {
			return errors.New("Jev api_format 必须是 typesafe 或 openrouter")
		}
		return check(&c.Models.Jev)
	case "hybrid":
		if c.Models.Jev.APIFormat != "typesafe" && c.Models.Jev.APIFormat != "openrouter" {
			return errors.New("Jev api_format 必须是 typesafe 或 openrouter")
		}
		if e := check(&c.Models.Jev); e != nil {
			return e
		}
		return check(&c.Models.LLM)
	default:
		return errors.New("只读或读写权限需要配置 review.mode：llm、jev 或 hybrid")
	}
}
func WithTimeout(ctx context.Context, seconds int64) (context.Context, context.CancelFunc) {
	if seconds == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}
