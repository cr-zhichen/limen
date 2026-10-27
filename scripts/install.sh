#!/bin/sh
set -eu

case "$(uname -s)" in
  Darwin) platform=darwin ;;
  Linux) platform=linux ;;
  *) echo '请使用 macOS、Linux 或 Windows 安装方式。' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo '当前仅提供 amd64 和 arm64 构建。' >&2; exit 1 ;;
esac

repo='https://github.com/cr-zhichen/limen'
# Follow the release redirect once to pin all downloads to the same version.
release=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
tag=${release##*/}
case "$tag" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo '无法获取正式版本。' >&2; exit 1 ;; esac
version=${tag#v}
archive="limen_${version}_${platform}_${arch}.tar.gz"
base="$repo/releases/download/$tag"
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT HUP INT TERM
curl -fsSL "$base/$archive" -o "$temp/$archive"
curl -fsSL "$base/SHA256SUMS" -o "$temp/SHA256SUMS"
expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$temp/SHA256SUMS")
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$temp/$archive" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$temp/$archive" | awk '{print $1}')
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo '文件校验失败，未安装。' >&2; exit 1
fi
tar -xzf "$temp/$archive" -C "$temp" limen
dest="${LIMEN_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dest"
install -m 755 "$temp/limen" "$dest/.limen-new"
mv -f "$dest/.limen-new" "$dest/limen"
"$dest/limen" version
case ":$PATH:" in
  *":$dest:"*) ;;
  *) printf '请将 %s 加入 PATH 后重新打开终端。\n' "$dest" ;;
esac
printf '运行 limen config init 开始配置，limen skill install 安装 AI skill。\n'
