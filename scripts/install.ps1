$ErrorActionPreference = 'Stop'
$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
if ($arch -eq 'x64') { $arch = 'amd64' }
if ($arch -notin @('amd64', 'arm64')) { throw '当前仅提供 x64 和 ARM64 构建。' }
$repo = 'https://github.com/cr-zhichen/limen'
$release = Invoke-RestMethod 'https://api.github.com/repos/cr-zhichen/limen/releases/latest'
$tag = $release.tag_name
if ($tag -notmatch '^v\d+\.\d+\.\d+$' -or $release.prerelease -or $release.draft) { throw '未找到正式版本。' }
$archive = "limen_$($tag.Substring(1))_windows_$arch.zip"
$temp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $temp | Out-Null
try {
    Invoke-WebRequest "$repo/releases/download/$tag/$archive" -OutFile "$temp/$archive"
    Invoke-WebRequest "$repo/releases/download/$tag/SHA256SUMS" -OutFile "$temp/SHA256SUMS"
    $line = Get-Content "$temp/SHA256SUMS" | Where-Object { ($_ -split '\s+')[1] -eq $archive }
    if (@($line).Count -ne 1) { throw '发布中缺少文件校验值。' }
    $expected = ($line -split '\s+')[0]
    if ((Get-FileHash "$temp/$archive" -Algorithm SHA256).Hash -ne $expected) { throw '文件校验失败，未安装。' }
    Expand-Archive "$temp/$archive" -DestinationPath "$temp/unpacked"
    $dest = if ($env:LIMEN_INSTALL_DIR) { $env:LIMEN_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Limen' }
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item "$temp/unpacked/limen.exe" "$dest/limen.exe" -Force
    & "$dest/limen.exe" version
    if ($LASTEXITCODE -ne 0) { throw '安装后的程序无法运行。' }
    Write-Output "请将 $dest 加入 PATH，然后运行 limen config init。"
} finally {
    Remove-Item $temp -Recurse -Force
}
