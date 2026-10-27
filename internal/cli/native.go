package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"

	"github.com/cr-zhichen/limen/internal/config"
	"github.com/cr-zhichen/limen/internal/files"
	"github.com/cr-zhichen/limen/internal/forward"
	"github.com/cr-zhichen/limen/internal/jumpserver"
	"github.com/cr-zhichen/limen/internal/remote"
	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
)

func dialTarget(ctx context.Context, c *config.Config, asset, account string) (*remote.Client, error) {
	api := jumpserver.New(&c.JumpServer)
	a, e := api.Asset(ctx, asset)
	if e != nil {
		return nil, e
	}
	token, e := api.Token(ctx, a, account)
	if e != nil {
		return nil, e
	}
	return remote.Connect(ctx, &c.JumpServer, token)
}
func targetFlags(cmd *cobra.Command, asset, account *string) {
	cmd.PersistentFlags().StringVar(asset, "asset", "", "资产 ID")
	cmd.PersistentFlags().StringVar(account, "account", "", "账号 alias")
	_ = cmd.MarkPersistentFlagRequired("asset")
	_ = cmd.MarkPersistentFlagRequired("account")
}
func filesCommand(load func() (*config.Config, error)) *cobra.Command {
	var asset, account string
	root := &cobra.Command{Use: "files", Short: "通过 SFTP 列出、上传或下载文件"}
	targetFlags(root, &asset, &account)
	for _, operation := range []string{"ls", "put", "get"} {
		var recursive bool
		cmd := &cobra.Command{Use: operation + " <来源路径> <目标路径>", Args: cobra.ExactArgs(2)}
		switch operation {
		case "ls":
			cmd.Use = "ls [远端目录]"
			cmd.Short = "列出远端目录"
			cmd.Args = cobra.MaximumNArgs(1)
		case "put":
			cmd.Use = "put <本地路径> <远端路径>"
			cmd.Short = "上传文件或目录"
		case "get":
			cmd.Use = "get <远端路径> <本地路径>"
			cmd.Short = "下载文件或目录"
		}
		if operation != "ls" {
			cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "传输整个目录，目标路径是目录本身")
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			c, e := load()
			if e != nil {
				return e
			}
			finish := automatic(cmd)
			defer finish()
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()
			opening, stop := config.WithTimeout(ctx, c.Session.ConnectTimeout)
			defer stop()
			conn, e := dialTarget(opening, c, asset, account)
			if e != nil {
				return e
			}
			defer conn.Close()
			stopOpen := context.AfterFunc(opening, func() { conn.Close() })
			client, e := sftp.NewClient(conn.SSH)
			stopOpen()
			if e != nil {
				return fmt.Errorf("SFTP 连接失败，请检查 JumpServer 文件传输授权：%w", e)
			}
			defer client.Close()
			stop()
			stopIO := context.AfterFunc(ctx, func() { conn.Close() })
			defer stopIO()
			if operation == "ls" {
				dir := "."
				if len(args) > 0 {
					dir = args[0]
				}
				entries, e := files.List(client, dir)
				if e != nil {
					return e
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
			}
			if operation == "put" {
				e = files.Upload(client, args[0], args[1], recursive)
			} else {
				e = files.Download(client, args[0], args[1], recursive)
			}
			if e != nil {
				return e
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"operation": operation, "source": args[0], "destination": args[1], "status": "complete"})
		}
		root.AddCommand(cmd)
	}
	return root
}

func forwardCommand(load func() (*config.Config, error)) *cobra.Command {
	var asset, account string
	var local, reverse, dynamic []string
	cmd := &cobra.Command{Use: "forward", Short: "保持本地、远端或 SOCKS5 端口转发", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if len(local)+len(reverse)+len(dynamic) == 0 {
			return errors.New("请指定 -L、-R 或 -D 转发")
		}
		type route struct {
			kind string
			spec forward.Spec
		}
		var routes []route
		for _, kind := range []string{"local", "remote"} {
			values := local
			if kind == "remote" {
				values = reverse
			}
			for _, v := range values {
				s, e := forward.Parse(v)
				if e != nil {
					return e
				}
				routes = append(routes, route{kind, s})
			}
		}
		for _, v := range dynamic {
			listen, e := forward.DynamicAddress(v)
			if e != nil {
				return e
			}
			routes = append(routes, route{"socks5", forward.Spec{Listen: listen}})
		}
		c, e := load()
		if e != nil {
			return e
		}
		finish := automatic(cmd)
		defer finish()
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()
		ctx, end := context.WithCancel(ctx)
		defer end()
		opening, stop := config.WithTimeout(ctx, c.Session.ConnectTimeout)
		defer stop()
		conn, e := dialTarget(opening, c, asset, account)
		if e != nil {
			return e
		}
		defer conn.Close()
		if e = conn.KeepOpen(opening); e != nil {
			return e
		}
		stopOpen := context.AfterFunc(opening, func() { conn.Close() })
		defer stopOpen()
		type bound struct {
			route
			listener net.Listener
			dial     forward.Dial
		}
		var listeners []bound
		for _, r := range routes {
			var listener net.Listener
			var e error
			dial := forward.Dial(conn.SSH.DialContext)
			if r.kind == "remote" {
				listener, e = conn.SSH.Listen("tcp", r.spec.Listen)
				dial = (&net.Dialer{}).DialContext
			} else {
				listener, e = net.Listen("tcp", r.spec.Listen)
			}
			if e != nil {
				return fmt.Errorf("无法建立 %s 转发，请检查地址和 JumpServer 转发权限：%w", r.kind, e)
			}
			defer listener.Close()
			listeners = append(listeners, bound{r, listener, dial})
		}
		stopOpen()
		stop()
		failures := make(chan error, len(listeners)+1)
		var wg sync.WaitGroup
		for _, b := range listeners {
			if e = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"type": "forwarding", "kind": b.kind, "listen": b.listener.Addr().String(), "target": b.spec.Target}); e != nil {
				return e
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				failures <- forward.Serve(ctx, b.listener, b.dial, b.spec.Target, b.kind == "socks5", func(e error) { fmt.Fprintln(cmd.ErrOrStderr(), "转发连接失败：", e) })
			}()
		}
		go func() { failures <- conn.Wait() }()
		select {
		case <-ctx.Done():
			e = nil
		case e = <-failures:
			if e == nil {
				e = errors.New("SSH 转发连接已关闭")
			}
		}
		end()
		conn.Close()
		wg.Wait()
		return e
	}}
	targetFlags(cmd, &asset, &account)
	cmd.Flags().StringArrayVarP(&local, "local", "L", nil, "[本地监听地址:]端口:远端目标主机:端口，可重复")
	cmd.Flags().StringArrayVarP(&reverse, "remote", "R", nil, "[远端监听地址:]端口:本地目标主机:端口，可重复")
	cmd.Flags().StringArrayVarP(&dynamic, "dynamic", "D", nil, "本地 SOCKS5 监听 [地址:]端口，可重复")
	return cmd
}
