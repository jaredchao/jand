#!/bin/sh
# jand installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | sh
#
# Downloads the release archive for this machine from GitHub, checks it
# against the release's SHA256SUMS.txt, installs the jand program, and then
# runs "jand setup" when there is a terminal to answer its questions.
#
# Environment:
#   JAND_VERSION      a release tag such as v0.4.3 (default: the newest release)
#   JAND_INSTALL_DIR  where to put jand (default: $HOME/.local/bin)
#   JAND_NO_SETUP=1   install only; do not run jand setup
set -eu

REPO="jaredchao/jand"
DIR="${JAND_INSTALL_DIR:-${HOME}/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf '安装失败：%s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=macos ;;
  Linux) os=linux ;;
  *) die "不支持的系统 $(uname -s)。Windows 请用 install.ps1。" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "不支持的处理器架构 $(uname -m)" ;;
esac
command -v curl >/dev/null 2>&1 || die "需要 curl"
command -v tar >/dev/null 2>&1 || die "需要 tar"
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "需要 sha256sum 或 shasum 来校验下载的文件"
fi

tag="${JAND_VERSION:-}"
if [ -z "${tag}" ]; then
  # Every release so far is a prerelease, which /releases/latest skips; the
  # list is newest first.
  tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases?per_page=1" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  [ -n "${tag}" ] || die "查不到 ${REPO} 的发行版本（GitHub API 可能暂时不可用，可用 JAND_VERSION=v0.4.3 指定）"
fi
version="${tag#v}"
name="jand-${version}-${os}-${arch}"
base="https://github.com/${REPO}/releases/download/${tag}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT INT TERM
say "下载 jand ${version}（${os}-${arch}）……"
curl -fsSL -o "${tmp}/${name}.tar.gz" "${base}/${name}.tar.gz" || die "下载 ${base}/${name}.tar.gz 失败"
curl -fsSL -o "${tmp}/SHA256SUMS.txt" "${base}/SHA256SUMS.txt" || die "下载校验文件失败"

want="$(awk -v f="${name}.tar.gz" '$2 == f { print $1 }' "${tmp}/SHA256SUMS.txt")"
[ -n "${want}" ] || die "校验文件里没有 ${name}.tar.gz"
got="$(sha "${tmp}/${name}.tar.gz")"
[ "${want}" = "${got}" ] || die "校验不一致：下载的文件可能损坏或被替换（期望 ${want}，实际 ${got}）"
say "校验通过（SHA-256 ${got}）"

tar -xzf "${tmp}/${name}.tar.gz" -C "${tmp}"
[ -f "${tmp}/${name}/jand" ] || die "压缩包里没有 jand 程序"
mkdir -p "${DIR}"
cp "${tmp}/${name}/jand" "${DIR}/jand.new"
chmod 755 "${DIR}/jand.new"
mv -f "${DIR}/jand.new" "${DIR}/jand"
say "已安装到 ${DIR}/jand（$("${DIR}/jand" --version)）"

case ":${PATH}:" in
  *":${DIR}:"*) cmd=jand ;;
  *)
    cmd="${DIR}/jand"
    case "${SHELL:-}" in
      */zsh) rc="${HOME}/.zshrc" ;;
      */bash) rc="${HOME}/.bashrc" ;;
      *) rc="你的 shell 配置文件" ;;
    esac
    say ""
    say "${DIR} 不在 PATH 上。把下面这行加进 ${rc}，然后重新打开终端："
    say "  export PATH=\"${DIR}:\${PATH}\""
    ;;
esac

if [ "${JAND_NO_SETUP:-}" = "1" ]; then
  say ""
  say "接下来运行：${cmd} setup"
elif [ -r /dev/tty ] && [ -w /dev/tty ] && "${DIR}/jand" help 2>/dev/null | grep -q "jand setup"; then
  say ""
  # The script itself arrives on stdin under curl | sh; answers come from the terminal.
  "${DIR}/jand" setup </dev/tty
else
  say ""
  say "接下来运行：${cmd} setup"
fi
