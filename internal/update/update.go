package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const Repository = "cr-zhichen/limen"

type Result struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	URL       string `json:"url"`
}
type Client struct {
	HTTP      *http.Client
	API       string
	Downloads string
	Cache     string
}

func New() *Client {
	cache, _ := os.UserCacheDir()
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, API: "https://api.github.com/repos/" + Repository + "/releases/latest", Downloads: "https://github.com/" + Repository + "/releases/download", Cache: filepath.Join(cache, "limen", "update.json")}
}
func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "limen")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("下载失败（HTTP %d），请检查网络或稍后重试", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("下载内容超过预期大小")
	}
	return b, nil
}
func (c *Client) Check(ctx context.Context, current string) (Result, error) {
	result := Result{Current: current}
	b, err := c.get(ctx, c.API, 1<<20)
	if err != nil {
		return result, err
	}
	var r struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(b, &r) != nil || !semver.IsValid(r.Tag) || semver.Prerelease(r.Tag) != "" || semver.Canonical(r.Tag) != r.Tag || r.Draft || r.Prerelease {
		return result, errors.New("没有可用的正式发布版本")
	}
	result.Latest = r.Tag
	result.URL = "https://github.com/" + Repository + "/releases/tag/" + r.Tag
	result.Available = current == "dev" || semver.Compare(r.Tag, "v"+strings.TrimPrefix(current, "v")) > 0
	return result, nil
}
func (c *Client) Automatic(ctx context.Context, current string, out io.Writer) {
	if current == "dev" || os.Getenv("LIMEN_NO_UPDATE_CHECK") == "1" {
		return
	}
	var cache struct {
		Checked time.Time `json:"checked"`
		Result  Result    `json:"result"`
	}
	b, _ := os.ReadFile(c.Cache)
	_ = json.Unmarshal(b, &cache)
	if time.Since(cache.Checked) >= 24*time.Hour {
		cache.Checked = time.Now()
		result, err := c.Check(ctx, current)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			cache.Result = result
		}
		if os.MkdirAll(filepath.Dir(c.Cache), 0700) == nil {
			b, _ = json.Marshal(cache)
			_ = os.WriteFile(c.Cache, b, 0600)
		}
	}
	if semver.IsValid(cache.Result.Latest) && semver.Compare(cache.Result.Latest, "v"+strings.TrimPrefix(current, "v")) > 0 {
		fmt.Fprintf(out, "发现 Limen %s，运行 limen update 更新。\n", cache.Result.Latest)
	}
}
func archiveName(tag, goos, arch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "limen_" + strings.TrimPrefix(tag, "v") + "_" + goos + "_" + arch + ext
}
func (c *Client) Install(ctx context.Context, r Result, target string) error {
	name := archiveName(r.Latest, runtime.GOOS, runtime.GOARCH)
	base := c.Downloads + "/" + r.Latest + "/"
	sums, err := c.get(ctx, base+"SHA256SUMS", 1<<20)
	if err != nil {
		return err
	}
	wanted := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			wanted = f[0]
		}
	}
	if len(wanted) != 64 {
		return errors.New("发布中缺少当前平台的文件校验值")
	}
	data, err := c.get(ctx, base+name, 64<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != wanted {
		return errors.New("下载文件校验失败，未安装更新")
	}
	executable := "limen"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	binary, err := unpack(data, name, executable)
	if err != nil {
		return err
	}
	return replace(target, binary)
}
func unpack(data []byte, name, executable string) ([]byte, error) {
	if strings.HasSuffix(name, ".zip") {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range z.File {
			if f.Name == executable && f.Mode().IsRegular() {
				r, e := f.Open()
				if e != nil {
					return nil, e
				}
				defer r.Close()
				return readBinary(r)
			}
		}
	} else {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, e := tr.Next()
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				return nil, e
			}
			if h.Name == executable && h.Typeflag == tar.TypeReg {
				return readBinary(tr)
			}
		}
	}
	return nil, errors.New("发布压缩包中没有 Limen 可执行文件")
}
func readBinary(r io.Reader) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, (64<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(b) == 0 || len(b) > 64<<20 {
		return nil, errors.New("可执行文件大小无效")
	}
	return b, nil
}
func replace(target string, data []byte) error {
	target, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".limen-update-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0755); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(tmp, target)
	}
	old := target + ".old"
	if err = os.Remove(old); err != nil && !os.IsNotExist(err) {
		return errors.New("旧版本仍在使用，请关闭其他 Limen 进程后更新")
	}
	if err = os.Rename(target, old); err != nil {
		return err
	}
	if err = os.Rename(tmp, target); err != nil {
		_ = os.Rename(old, target)
		return err
	}
	_ = os.Remove(old)
	return nil
}
