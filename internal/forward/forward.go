package forward

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

type Spec struct{ Listen, Target string }
type Dial func(context.Context, string, string) (net.Conn, error)

func Parse(spec string) (Spec, error) {
	var fields []string
	start := 0
	bracket := false
	for i, r := range spec {
		switch r {
		case '[':
			bracket = true
		case ']':
			bracket = false
		case ':':
			if !bracket {
				fields = append(fields, spec[start:i])
				start = i + 1
			}
		}
	}
	fields = append(fields, spec[start:])
	if len(fields) == 3 {
		fields = append([]string{"127.0.0.1"}, fields...)
	}
	if len(fields) != 4 {
		return Spec{}, errors.New("转发格式为 [监听地址:]监听端口:目标主机:目标端口")
	}
	for _, i := range []int{1, 3} {
		port, e := strconv.Atoi(fields[i])
		if e != nil || port < 0 || port > 65535 || (i == 3 && port == 0) {
			return Spec{}, errors.New("转发端口无效")
		}
	}
	host := strings.Trim(fields[0], "[]")
	if host == "" {
		host = "127.0.0.1"
	}
	target := strings.Trim(fields[2], "[]")
	if target == "" {
		return Spec{}, errors.New("转发目标主机不能为空")
	}
	return Spec{net.JoinHostPort(host, fields[1]), net.JoinHostPort(target, fields[3])}, nil
}
func DynamicAddress(value string) (string, error) {
	if _, e := strconv.Atoi(value); e == nil {
		value = net.JoinHostPort("127.0.0.1", value)
	}
	_, port, e := net.SplitHostPort(value)
	if e != nil {
		return "", errors.New("SOCKS 监听格式为 [地址:]端口")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 65535 {
		return "", errors.New("SOCKS 监听端口无效")
	}
	return value, nil
}

// Serve owns one listener. Both legs are closed when the CLI context ends.
func Serve(ctx context.Context, listener net.Listener, dial Dial, target string, dynamic bool, report func(error)) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	defer listener.Close()
	var active sync.WaitGroup
	defer func() { cancel(); active.Wait() }()
	for {
		conn, e := listener.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		active.Add(1)
		go func() {
			defer active.Done()
			defer conn.Close()
			cancel := context.AfterFunc(ctx, func() { conn.Close() })
			defer cancel()
			destination := target
			if dynamic {
				var e error
				destination, e = socksRequest(conn)
				if e != nil {
					if report != nil {
						report(e)
					}
					return
				}
			}
			upstream, e := dial(ctx, "tcp", destination)
			if e != nil {
				if dynamic {
					conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
				}
				if report != nil {
					report(e)
				}
				return
			}
			defer upstream.Close()
			cancelUp := context.AfterFunc(ctx, func() { upstream.Close() })
			defer cancelUp()
			if dynamic {
				if _, e = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); e != nil {
					return
				}
			}
			copyBoth(conn, upstream)
		}()
	}
}
func copyBoth(a, b net.Conn) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(a, b)
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(b, a)
	if c, ok := b.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	}
	<-done
}
func socksRequest(conn net.Conn) (string, error) {
	var header [2]byte
	if _, e := io.ReadFull(conn, header[:]); e != nil {
		return "", e
	}
	if header[0] != 5 {
		return "", errors.New("动态转发使用 SOCKS5")
	}
	methods := make([]byte, int(header[1]))
	if _, e := io.ReadFull(conn, methods); e != nil {
		return "", e
	}
	noAuth := false
	for _, m := range methods {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		conn.Write([]byte{5, 255})
		return "", errors.New("SOCKS 客户端未提供无认证方式")
	}
	if _, e := conn.Write([]byte{5, 0}); e != nil {
		return "", e
	}
	var req [4]byte
	if _, e := io.ReadFull(conn, req[:]); e != nil {
		return "", e
	}
	if req[0] != 5 || req[1] != 1 || req[2] != 0 {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return "", errors.New("SOCKS5 仅支持 TCP CONNECT")
	}
	size := 0
	switch req[3] {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		var length [1]byte
		if _, e := io.ReadFull(conn, length[:]); e != nil {
			return "", e
		}
		size = int(length[0])
	default:
		return "", errors.New("无效的 SOCKS5 地址")
	}
	host := make([]byte, size)
	if _, e := io.ReadFull(conn, host); e != nil {
		return "", e
	}
	var p [2]byte
	if _, e := io.ReadFull(conn, p[:]); e != nil {
		return "", e
	}
	name := string(host)
	if req[3] != 3 {
		name = net.IP(host).String()
	}
	if name == "" || binary.BigEndian.Uint16(p[:]) == 0 {
		return "", errors.New("无效的 SOCKS5 目标")
	}
	return net.JoinHostPort(name, fmt.Sprint(binary.BigEndian.Uint16(p[:]))), nil
}
