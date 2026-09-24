# jand installer for Windows (PowerShell 5.1 or later).
#
#   irm https://raw.githubusercontent.com/jaredchao/jand/main/install.ps1 | iex
#
# Downloads the Windows release archive from GitHub, checks it against the
# release's SHA256SUMS.txt, installs jand.exe for the current user, adds it
# to the user PATH, and runs "jand setup".
#
# Environment:
#   JAND_VERSION      a release tag such as v0.4.3 (default: the newest release)
#   JAND_INSTALL_DIR  where to put jand.exe (default: %LOCALAPPDATA%\Programs\jand)
#   JAND_NO_SETUP=1   install only; do not run jand setup
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'jaredchao/jand'
$dir = if ($env:JAND_INSTALL_DIR) { $env:JAND_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\jand' }
if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64') {
    throw "安装失败：目前只有 windows-amd64 的发行包，本机是 $($env:PROCESSOR_ARCHITECTURE)"
}

$tag = $env:JAND_VERSION
if (-not $tag) {
    # Every release so far is a prerelease, which /releases/latest skips.
    $releases = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases?per_page=1" -Headers @{ 'User-Agent' = 'jand-installer' }
    $tag = @($releases)[0].tag_name
    if (-not $tag) { throw "安装失败：查不到 $repo 的发行版本（可用 JAND_VERSION=v0.4.3 指定）" }
}
$version = $tag -replace '^v', ''
$name = "jand-$version-windows-amd64"
$base = "https://github.com/$repo/releases/download/$tag"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("jand-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "下载 jand $version（windows-amd64）……"
    Invoke-WebRequest -Uri "$base/$name.zip" -OutFile "$tmp\$name.zip" -UseBasicParsing
    Invoke-WebRequest -Uri "$base/SHA256SUMS.txt" -OutFile "$tmp\SHA256SUMS.txt" -UseBasicParsing

    $want = (Get-Content "$tmp\SHA256SUMS.txt" | Where-Object { ($_ -split '\s+')[1] -eq "$name.zip" } | ForEach-Object { ($_ -split '\s+')[0] }) | Select-Object -First 1
    if (-not $want) { throw "安装失败：校验文件里没有 $name.zip" }
    $got = (Get-FileHash -Algorithm SHA256 "$tmp\$name.zip").Hash.ToLower()
    if ($want -ne $got) { throw "安装失败：校验不一致，下载的文件可能损坏或被替换（期望 $want，实际 $got）" }
    Write-Host "校验通过（SHA-256 $got）"

    Expand-Archive -Path "$tmp\$name.zip" -DestinationPath $tmp -Force
    $exe = Join-Path $tmp "$name\jand.exe"
    if (-not (Test-Path $exe)) { throw '安装失败：压缩包里没有 jand.exe' }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Copy-Item $exe (Join-Path $dir 'jand.exe') -Force
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
$jand = Join-Path $dir 'jand.exe'
Write-Host "已安装到 $jand（$(& $jand --version)）"

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($userPath -split ';') -contains $dir)) {
    [Environment]::SetEnvironmentVariable('Path', ($(if ($userPath) { "$userPath;" } else { '' }) + $dir), 'User')
    Write-Host "已把 $dir 加入用户 PATH；新开的终端里可以直接用 jand。"
}
$env:Path = "$env:Path;$dir"

if ($env:JAND_NO_SETUP -eq '1' -or -not ((& $jand help) -match 'jand setup')) {
    Write-Host '接下来运行：jand setup'
} else {
    Write-Host ''
    & $jand setup
}
