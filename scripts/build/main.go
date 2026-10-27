package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"
)

type target struct{ OS, Arch string }

var targets = []target{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func git(args ...string) (string, error) {
	b, e := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(b)), e
}
func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("请选择 local、package 或 release")
	}
	mode := os.Args[1]
	commit, e := git("rev-parse", "HEAD")
	if e != nil {
		return e
	}
	version := "dev"
	if mode == "release" {
		tag := os.Getenv("RELEASE_TAG")
		if !semver.IsValid(tag) || semver.Canonical(tag) != tag || semver.Prerelease(tag) != "" {
			return fmt.Errorf("RELEASE_TAG 必须是 vX.Y.Z 正式版")
		}
		head, e := git("rev-parse", "refs/tags/"+tag+"^{commit}")
		if e != nil || head != commit {
			return fmt.Errorf("发布标签必须指向 HEAD")
		}
		status, e := git("status", "--porcelain")
		if e != nil || status != "" {
			return fmt.Errorf("发布构建要求工作区干净")
		}
		version = strings.TrimPrefix(tag, "v")
	}
	if mode == "local" {
		return build(filepath.Join("bin", binary(runtime.GOOS)), target{runtime.GOOS, runtime.GOARCH}, version, commit)
	}
	if mode != "package" && mode != "release" {
		return fmt.Errorf("未知构建模式")
	}
	if e = os.MkdirAll("dist", 0755); e != nil {
		return e
	}
	var sums strings.Builder
	for _, t := range targets {
		tmp, e := os.MkdirTemp("", "limen-package-")
		if e != nil {
			return e
		}
		executable := filepath.Join(tmp, binary(t.OS))
		e = build(executable, t, version, commit)
		if e != nil {
			os.RemoveAll(tmp)
			return e
		}
		name := "limen_" + version + "_" + t.OS + "_" + t.Arch
		ext := ".tar.gz"
		if t.OS == "windows" {
			ext = ".zip"
		}
		name += ext
		files := map[string]string{binary(t.OS): executable, "README.md": "README.md", "LICENSE": "LICENSE", "config.example.toml": "config.example.toml", "skill/limen/SKILL.md": ".agents/skills/limen/SKILL.md"}
		e = pack(filepath.Join("dist", name), files, t.OS == "windows")
		os.RemoveAll(tmp)
		if e != nil {
			return e
		}
		data, e := os.ReadFile(filepath.Join("dist", name))
		if e != nil {
			return e
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	for _, file := range []string{"install.sh", "install.ps1"} {
		data, e := os.ReadFile(filepath.Join("scripts", file))
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join("dist", file), data, 0644); e != nil {
			return e
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), file)
	}
	return os.WriteFile("dist/SHA256SUMS", []byte(sums.String()), 0644)
}
func binary(goos string) string {
	if goos == "windows" {
		return "limen.exe"
	}
	return "limen"
}
func build(dest string, t target, version, commit string) error {
	if e := os.MkdirAll(filepath.Dir(dest), 0755); e != nil {
		return e
	}
	flags := "-s -w -X github.com/cr-zhichen/limen/internal/version.Version=" + version + " -X github.com/cr-zhichen/limen/internal/version.Commit=" + commit
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", flags, "-o", dest, "./cmd/limen")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+t.OS, "GOARCH="+t.Arch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
func pack(dest string, files map[string]string, windows bool) error {
	out, e := os.Create(dest)
	if e != nil {
		return e
	}
	defer out.Close()
	if windows {
		zw := zip.NewWriter(out)
		for name, src := range files {
			data, e := os.ReadFile(src)
			if e != nil {
				return e
			}
			h := &zip.FileHeader{Name: name, Method: zip.Deflate}
			h.SetMode(0644)
			if name == "limen.exe" {
				h.SetMode(0755)
			}
			w, e := zw.CreateHeader(h)
			if e != nil {
				return e
			}
			if _, e = w.Write(data); e != nil {
				return e
			}
		}
		return zw.Close()
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	for name, src := range files {
		f, e := os.Open(src)
		if e != nil {
			return e
		}
		stat, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		h := &tar.Header{Name: name, Size: stat.Size(), Mode: 0644}
		if name == "limen" {
			h.Mode = 0755
		}
		if e = tw.WriteHeader(h); e != nil {
			f.Close()
			return e
		}
		_, e = io.Copy(tw, f)
		f.Close()
		if e != nil {
			return e
		}
	}
	if e = tw.Close(); e != nil {
		return e
	}
	return gz.Close()
}
