package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cr-zhichen/limen"
	"github.com/cr-zhichen/limen/internal/config"
	"github.com/cr-zhichen/limen/internal/jumpserver"
	"github.com/cr-zhichen/limen/internal/remote"
	"github.com/cr-zhichen/limen/internal/review"
	"github.com/cr-zhichen/limen/internal/session"
	"github.com/cr-zhichen/limen/internal/update"
	"github.com/cr-zhichen/limen/internal/version"
	"github.com/spf13/cobra"
)

func New() *cobra.Command {
	var file string
	root := &cobra.Command{Use: "limen", Short: "通过 JumpServer 审核命令、传输文件和转发端口", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&file, "config", config.DefaultPath(), "配置文件路径")
	load := func() (*config.Config, error) { return config.Load(file) }
	root.AddCommand(&cobra.Command{Use: "version", Short: "显示版本", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"version": version.Version, "commit": version.Commit})
	}})
	cfg := &cobra.Command{Use: "config", Short: "创建或查看配置示例"}
	cfg.AddCommand(&cobra.Command{Use: "example", Short: "输出完整配置示例", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, e := fmt.Fprint(cmd.OutOrStdout(), limen.ConfigExample)
		return e
	}})
	cfg.AddCommand(&cobra.Command{Use: "init", Short: "创建配置文件", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if e := os.MkdirAll(filepath.Dir(file), 0700); e != nil {
			return e
		}
		f, e := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return fmt.Errorf("配置未创建，请检查目标是否已存在：%w", e)
		}
		defer f.Close()
		if _, e = f.WriteString(limen.ConfigExample); e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), file)
		return nil
	}})
	cfg.AddCommand(&cobra.Command{Use: "check", Short: "检查配置（不读取凭据）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, e := load()
		if e == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "配置有效")
		}
		return e
	}})
	root.AddCommand(cfg, skillCommand(), updateCommand())
	for _, name := range []string{"assets", "accounts"} {
		c := &cobra.Command{Use: name, Short: "列出授权资产", Args: cobra.NoArgs}
		if name == "accounts" {
			c.Use = "accounts <资产ID>"
			c.Short = "列出资产的可连接账号"
			c.Args = cobra.ExactArgs(1)
		}
		c.RunE = func(cmd *cobra.Command, args []string) error {
			conf, e := load()
			if e != nil {
				return e
			}
			finish := automatic(cmd)
			defer finish()
			ctx, cancel := config.WithTimeout(cmd.Context(), conf.Session.ConnectTimeout)
			defer cancel()
			client := jumpserver.New(&conf.JumpServer)
			if len(args) > 0 {
				asset, e := client.Asset(ctx, args[0])
				if e != nil {
					return e
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(asset.Accounts)
			}
			assets, e := client.Assets(ctx)
			if e != nil {
				return e
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(assets)
		}
		root.AddCommand(c)
	}
	var trust string
	host := &cobra.Command{Use: "host-key", Short: "查看 SSH 主机指纹或保存已核实的密钥", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		conf, e := load()
		if e != nil {
			return e
		}
		ctx, cancel := config.WithTimeout(cmd.Context(), conf.Session.ConnectTimeout)
		defer cancel()
		fp, e := remote.Probe(ctx, &conf.JumpServer, trust)
		if fp != "" {
			fmt.Fprintln(cmd.OutOrStdout(), fp)
		}
		return e
	}}
	host.Flags().StringVar(&trust, "trust", "", "已通过可信来源核实的 SHA256 指纹")
	root.AddCommand(host)
	root.AddCommand(connectCommand(load, false), connectCommand(load, true))
	root.AddCommand(filesCommand(load), forwardCommand(load))
	return root
}
func automatic(cmd *cobra.Command) func() {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); update.New().Automatic(ctx, version.Version, cmd.ErrOrStderr()) }()
	return func() { <-done; cancel() }
}

func connectCommand(load func() (*config.Config, error), once bool) *cobra.Command {
	var asset, account, permission, command, cwd string
	name, short := "connect", "保持连接，逐行接收命令请求"
	if once {
		name, short = "exec", "连接并执行一条命令，完成后正常关闭"
	}
	cmd := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := load()
		if err != nil {
			return err
		}
		mode := permission
		if mode == "" {
			mode = c.Permissions.Default
		}
		if !slices.Contains(c.Permissions.Allowed, mode) {
			return errors.New("申请的权限不在 permissions.allowed 中")
		}
		finish := automatic(cmd)
		defer finish()
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()
		opening, stop := config.WithTimeout(ctx, c.Session.ConnectTimeout)
		defer stop()
		conn, err := dialTarget(opening, c, asset, account)
		if err != nil {
			return err
		}
		defer conn.Close()
		if err = conn.Prepare(opening, c.Permissions.WritableDir); err != nil {
			return err
		}
		stop()

		alive, disconnect := context.WithCancelCause(ctx)
		defer disconnect(nil)
		go func() { _ = conn.SSH.Wait(); disconnect(errors.New("SSH 已断开，可写目录内容已保留")) }()
		out := &session.Output{Encoder: json.NewEncoder(cmd.OutOrStdout())}
		if err = out.Send(session.Event{Type: "connected", WritableDir: conn.Writable}); err != nil {
			return err
		}
		runner := &session.Runner{Remote: conn, Reviewer: &review.Engine{Config: c}, Mode: mode, Settings: c.Session, Output: out}
		if once {
			err = runner.Exec(alive, session.Request{ID: "1", Op: "exec", Command: command, Cwd: cwd})
			var commandError *session.CommandError
			if err != nil && !errors.As(err, &commandError) {
				return err
			}
			if cleanupErr := runner.Finish(alive); cleanupErr != nil {
				return cleanupErr
			}
			return err
		}
		err = runner.Serve(alive, cmd.InOrStdin())
		// SIGINT while idle is a normal close; during a command the transport may
		// already be closed, so cleanup failure is reported instead of hidden.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil && cmd.Context().Err() == nil {
			return runner.Finish(context.Background())
		}

		if alive.Err() != nil {
			return context.Cause(alive)
		}
		return err
	}}
	cmd.Flags().StringVar(&asset, "asset", "", "资产 ID（由 limen assets 获取）")
	cmd.Flags().StringVar(&account, "account", "", "账号 alias（由 limen accounts 获取）")
	cmd.Flags().StringVar(&permission, "permission", "", "申请 read-only、read-write 或 unrestricted")
	_ = cmd.MarkFlagRequired("asset")
	_ = cmd.MarkFlagRequired("account")
	if once {
		cmd.Flags().StringVar(&command, "command", "", "完整远端命令")
		cmd.Flags().StringVar(&cwd, "cwd", "", "本条命令的远端执行目录，默认账号主目录")
		_ = cmd.MarkFlagRequired("command")
	}
	return cmd
}
func skillCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "skill", Short: "读取当前版本的 AI 使用说明", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, e := fmt.Fprint(cmd.OutOrStdout(), strings.ReplaceAll(limen.Skill, "{{version}}", version.Version))
		return e
	}}
	var dir string
	var force bool
	install := &cobra.Command{Use: "install", Short: "安装 AI skill 入口", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if dir == "" {
			h, e := os.UserHomeDir()
			if e != nil {
				return e
			}
			dir = filepath.Join(h, ".agents", "skills")
		}
		dst := filepath.Join(dir, "limen", "SKILL.md")
		if e := os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
			return e
		}
		flags := os.O_CREATE | os.O_WRONLY | os.O_EXCL
		if force {
			flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		}
		f, e := os.OpenFile(dst, flags, 0644)
		if e != nil {
			return fmt.Errorf("skill 未写入；如需替换已有入口，请使用 --force：%w", e)
		}
		defer f.Close()
		if _, e = f.WriteString(limen.SkillStub); e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), dst)
		return nil
	}}
	install.Flags().StringVar(&dir, "dir", "", "AI 工具的 skills 根目录，默认 ~/.agents/skills")
	install.Flags().BoolVar(&force, "force", false, "替换已有的 Limen skill 入口")
	cmd.AddCommand(install)
	return cmd
}
func updateCommand() *cobra.Command {
	var check bool
	cmd := &cobra.Command{Use: "update", Short: "检查并安装最新正式版", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client := update.New()
		r, err := client.Check(cmd.Context(), version.Version)
		if err != nil {
			return err
		}
		if !check && r.Available {
			target, e := os.Executable()
			if e != nil {
				return e
			}
			if e = client.Install(cmd.Context(), r, target); e != nil {
				return e
			}
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	}}
	cmd.Flags().BoolVar(&check, "check", false, "只检查，不下载安装")
	return cmd
}
