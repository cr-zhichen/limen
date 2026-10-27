package files

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
)

type Entry struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Mode      string `json:"mode"`
	Directory bool   `json:"directory"`
}

func List(c *sftp.Client, remote string) ([]Entry, error) {
	list, e := c.ReadDir(remote)
	if e != nil {
		return nil, e
	}
	entries := make([]Entry, 0, len(list))
	for _, f := range list {
		entries = append(entries, Entry{f.Name(), f.Size(), f.Mode().String(), f.IsDir()})
	}
	return entries, nil
}
func Upload(c *sftp.Client, local, remote string, recursive bool) error {
	info, e := os.Lstat(local)
	if e != nil {
		return e
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("请指定符号链接对应的实际文件：%s", local)
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("上传目录请添加 --recursive")
		}
		if e = c.MkdirAll(remote); e != nil {
			return e
		}
		entries, e := os.ReadDir(local)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			if e = Upload(c, filepath.Join(local, entry.Name()), path.Join(remote, entry.Name()), true); e != nil {
				return e
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("只能上传普通文件或目录：%s", local)
	}
	src, e := os.Open(local)
	if e != nil {
		return e
	}
	defer src.Close()
	var mode os.FileMode
	existing, e := c.Lstat(remote)
	replacing := e == nil
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if replacing {
		if !existing.Mode().IsRegular() {
			return errors.New("上传目标不是普通文件，请指定实际文件路径")
		}
		mode = existing.Mode().Perm()
		if _, ok := c.HasExtension("posix-rename@openssh.com"); !ok {
			return errors.New("服务器不支持完整传输后替换已有文件，请上传到一个新文件名")
		}
	}
	tmp := path.Join(path.Dir(remote), ".limen-upload-"+rand.Text())
	dst, e := c.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return e
	}
	defer c.Remove(tmp)
	if !replacing {
		created, err := dst.Stat()
		if err != nil {
			dst.Close()
			return err
		}
		mode = created.Mode().Perm()
	}
	if e = c.Chmod(tmp, 0600); e != nil {
		dst.Close()
		return e
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if replacing {
		if stat, ok := existing.Sys().(*sftp.FileStat); ok {
			if e = c.Chown(tmp, int(stat.UID), int(stat.GID)); e != nil {
				return e
			}
		}
	}
	if e = c.Chmod(tmp, mode); e != nil {
		return e
	}
	if replacing {
		return c.PosixRename(tmp, remote)
	}
	return c.Rename(tmp, remote)
}
func Download(c *sftp.Client, remote, local string, recursive bool) error {
	info, e := c.Lstat(remote)
	if e != nil {
		return e
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("请指定符号链接对应的实际文件：%s", remote)
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("下载目录请添加 --recursive")
		}
		if e = os.MkdirAll(local, 0700); e != nil {
			return e
		}
		entries, e := c.ReadDir(remote)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			name := entry.Name()
			if !filepath.IsLocal(name) || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
				return fmt.Errorf("服务器返回了无效的文件名")
			}
			// A pre-existing local link must not turn a downloaded directory tree into
			// writes outside the user's selected destination.
			dst := filepath.Join(local, name)
			if stat, e := os.Lstat(dst); e == nil && stat.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("下载目标含有符号链接，请更换目录：%s", dst)
			}
			if e = Download(c, path.Join(remote, name), dst, true); e != nil {
				return e
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("只能下载普通文件或目录：%s", remote)
	}
	src, e := c.Open(remote)
	if e != nil {
		return e
	}
	defer src.Close()
	mode := info.Mode().Perm() & 0700
	if existing, err := os.Lstat(local); err == nil {
		if !existing.Mode().IsRegular() {
			return errors.New("下载目标不是普通文件，请指定实际文件路径")
		}
		mode = existing.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	dst, e := os.CreateTemp(filepath.Dir(local), ".limen-download-*")
	if e != nil {
		return e
	}
	tmp := dst.Name()
	defer os.Remove(tmp)
	if _, e = io.Copy(dst, src); e != nil {
		dst.Close()
		return e
	}
	if e = dst.Chmod(mode); e != nil {
		dst.Close()
		return e
	}
	if e = dst.Close(); e != nil {
		return e
	}
	return os.Rename(tmp, local)
}
