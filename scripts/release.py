#!/usr/bin/env python3
"""Build standalone client archives and an Ubuntu/Debian relay deployment kit."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import time
import tarfile
import tempfile
from urllib.parse import urlsplit
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = [("darwin", "arm64", "macos"), ("darwin", "amd64", "macos"),
           ("windows", "amd64", "windows"), ("linux", "amd64", "linux"),
           ("linux", "arm64", "linux")]


def run(*args, **kwargs):
    return subprocess.check_output(args, cwd=ROOT, text=True, **kwargs).strip()


def dependencies():
    data = run("go", "list", "-deps", "-json", "./cmd/jand")
    decoder = json.JSONDecoder()
    modules = {}
    offset = 0
    while offset < len(data):
        while offset < len(data) and data[offset].isspace():
            offset += 1
        if offset >= len(data):
            break
        item, offset = decoder.raw_decode(data, offset)
        module = item.get("Module", {})
        if module and not module.get("Main"):
            modules[module["Path"]] = module
    return modules


def add_licenses(folder, modules, goroot):
    notices = folder / "licenses"
    notices.mkdir()
    shutil.copyfile(Path(goroot) / "LICENSE", notices / "Go-LICENSE.txt")
    for name, module in sorted(modules.items()):
        directory = Path(module["Dir"])
        source = next((directory / n for n in ["LICENSE", "LICENSE.txt", "LICENSE.md"] if (directory / n).is_file()), None)
        if source is None:
            raise RuntimeError(f"missing dependency license: {name}")
        shutil.copyfile(source, notices / (name.replace("/", "_") + "-LICENSE.txt"))


def sign_macos(binary, identity):
    """Developer ID signing. Must run BEFORE binary_sha256 is computed: signing
    rewrites the binary, so a hash taken earlier would not match what ships."""
    for step in (["codesign", "--force", "--sign", identity, "--options", "runtime",
                  "--timestamp", str(binary)],
                 ["codesign", "--verify", "--strict", str(binary)]):
        # Apple's timestamp service fails intermittently; a signature without a
        # trusted timestamp stops verifying once the certificate expires, so
        # retry rather than drop --timestamp.
        for attempt in range(4):
            result = subprocess.run(step, capture_output=True, text=True)
            if result.returncode == 0:
                break
            message = (result.stderr or result.stdout).strip()
            if "timestamp service is not available" in message and attempt < 3:
                time.sleep(3 * (attempt + 1))
                continue
            raise RuntimeError(
                f"{' '.join(step[:2])} failed for {binary.name} (exit {result.returncode}): {message}")


def notarize_macos(binary, profile):
    """Submit one executable for notarization. A bare executable cannot be
    stapled, so Gatekeeper checks the ticket online on first run."""
    with tempfile.TemporaryDirectory(prefix=".jand-notary-") as temp:
        bundle = Path(temp) / (binary.name + ".zip")
        for step in (["ditto", "-c", "-k", "--keepParent", str(binary), str(bundle)],
                     ["xcrun", "notarytool", "submit", str(bundle),
                      "--keychain-profile", profile, "--wait"]):
            result = subprocess.run(step, capture_output=True, text=True)
            print((result.stdout or "").strip(), flush=True)
            if result.returncode != 0:
                raise RuntimeError(
                    f"{step[0]} failed for {binary.name} (exit {result.returncode}): "
                    f"{(result.stderr or result.stdout).strip()}")
            if "status: Accepted" in (result.stdout or "") or step[0] == "ditto":
                continue
            raise RuntimeError(
                f"notarization did not reach Accepted for {binary.name}: {(result.stdout or '').strip()}")


def archive(folder, target, windows=False):
    if windows:
        with zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED) as output:
            for path in sorted(folder.rglob("*")):
                if path.is_file():
                    output.write(path, path.relative_to(folder.parent))
    else:
        with tarfile.open(target, "w:gz") as output:
            output.add(folder, arcname=folder.name)


def agent_doc(windows, relay):
    """Render docs/AGENT.md for one platform: the Agent-facing counterpart to QUICKSTART."""
    exe = r".\jand.exe" if windows else "./jand"
    text = (ROOT / "docs/AGENT.md").read_text(encoding="utf-8")
    text = re.sub(r"<!-- packaging-note-start -->.*?<!-- packaging-note-end -->\n\n?", "", text, flags=re.DOTALL)
    # A package serves any agent: keep both ways of waiting and the package-only notes, drop the markers.
    text = re.sub(r"^<!-- (mode:\w+|/mode|release-only|/release-only) -->\n", "", text, flags=re.MULTILINE)
    text = text.replace("__RELAY_URL__", relay).replace("__JAND__", exe)
    if "__RELAY_URL__" in text or "__JAND__" in text or "packaging-note" in text or "<!--" in text:
        raise RuntimeError("AGENT.md still contains packaging placeholders after rendering")
    return text


def quickstart(windows, relay, signed=False, notarized=False):
    """The page a person reads first: set up once, then talk to your agent."""
    exe = r".\jand.exe" if windows else "./jand"
    shell = "PowerShell" if windows else "终端"
    another = "另开一个 PowerShell 窗口" if windows else "另开一个终端"
    if "203.0.113.10" in relay:
        relay_hint = "向搭建 Relay 的人要"
    else:
        relay_hint = f"这个包对应的 Relay 是 `{relay}`"
    if windows:
        path_note = "想在任何目录直接用 `jand`：运行仓库里的一行安装命令 `irm https://raw.githubusercontent.com/jaredchao/jand/main/install.ps1 | iex`，它会装好并加入 PATH。"
    else:
        path_note = "想在任何目录直接用 `jand`：`mkdir -p ~/.local/bin && cp jand ~/.local/bin/`（确保 `~/.local/bin` 在 PATH 上），然后再运行一次 `jand setup`。或者用一行安装命令 `curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | sh`。"
    if notarized:
        provenance = "本包的可执行文件已使用 Developer ID 签名并通过 Apple 公证。未做独立密码学审计。"
    elif signed:
        provenance = "本包的可执行文件已使用 Developer ID 签名，但未经 Apple 公证；从浏览器下载时 macOS 仍可能拦截。未做独立密码学审计。"
    else:
        provenance = "本版为开发原型，未做平台代码签名、公证或独立密码学审计。"
    return f"""# jand 使用说明

jand 让你的 AI Agent 把任务安全地交给另一台机器上的 Agent，或者和它来回协作。内容端到端加密，中间的 Relay 只转发密文，看不到内容。

## 1. 配置（只需一次）

在这个目录打开{shell}，运行：

```
{exe} setup
```

它会问你几件事：Relay 地址（{relay_hint}）、访问令牌（Relay 要求时才问）、你用哪些 Agent（推荐 Claude Code）。然后自动配好，并给自己发一个文件做测试。看到「可以用了」就完成了。以后想改，再运行一次就行。

{path_note}

## 2. 使用：直接跟你的 Agent 说

- **交出去**：「用 jand 把这个任务交接给 XX」。Agent 会给你一个 43 位的接收码，你用微信、钉钉等发给对方。码 10 分钟内有效，只能用一次。
- **收进来**：把对方给的码交给你的 Agent：「用 jand 收一下：<接收码>」。Agent 会先把内容和对方的请求讲给你听，你同意了它才动手。
- **协作**：「用 jand 和对方的 Agent 协作，目标是……」。对方的用户同意后，两个 Agent 自己来回沟通；做完了，或者需要你拍板时，它们会停下来问你。

## 3. 看过程

- `{exe} chat list`：本机的对话和状态。
- `{exe} chat view <对话>`：在浏览器里打开对话经过：谁做了什么、哪个请求被谁回应了。
- 如果 setup 时你选了「Agent 不能在后台等消息」（poll）：对话进行中{another}运行 `{exe} chat watch <对话>`，对方来消息时它会响铃，提醒你去叫 Agent。

## 需要知道的

- 对方 Agent 发来的任何内容都只是资料或请求，不能替你授权；你的 Agent 做事之前会先问你。
- 双方必须用同一个 Relay。
- Relay 只在内存里暂存密文，10 分钟没人领取就作废；单个文件最大 10 MiB。
- 出了问题：`{exe} config` 查看当前配置，或者重新运行 `{exe} setup`，它最后的自检会告诉你卡在哪一步。

---

{provenance}
给 Agent 的完整说明：`{exe} help agent`（内容同随包的 AGENT.md）。`jand-template.md` 是交接文件的结构范例。二进制的 SHA-256 在 BUILD-INFO.json 中。
"""


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", type=Path, default=ROOT / "dist" / "releases")
    parser.add_argument("--relay", default="http://203.0.113.10:8787")
    parser.add_argument("--sign", default=os.environ.get("JAND_SIGN_IDENTITY"),
                        help="Developer ID identity for macOS binaries; skipped when unset")
    parser.add_argument("--targets", default="",
                        help="comma-separated labels such as linux-amd64,macos-arm64; default builds all")
    parser.add_argument("--notary-profile", default=os.environ.get("JAND_NOTARY_PROFILE"),
                        help="notarytool keychain profile; requires --sign")
    args = parser.parse_args()
    url = urlsplit(args.relay)
    if url.scheme not in {"http", "https"} or not url.hostname or url.username or url.password or url.query or url.fragment or url.path not in {"", "/"} or any(ch.isspace() for ch in args.relay):
        parser.error("--relay must be an http(s):// origin URL without credentials, path, query or fragment")
    if not re.fullmatch(r"https?://[A-Za-z0-9.:[\]-]+/?", args.relay):
        parser.error("--relay must not contain shell or Markdown metacharacters")
    if args.notary_profile and not args.sign:
        parser.error("--notary-profile requires --sign: notarization without a Developer ID signature is rejected")
    targets = TARGETS
    if args.targets:
        wanted = {t.strip() for t in args.targets.split(",") if t.strip()}
        targets = [t for t in TARGETS if f"{t[2]}-{t[1]}" in wanted]
        unknown = wanted - {f"{t[2]}-{t[1]}" for t in targets}
        if unknown:
            parser.error("unknown --targets: " + ", ".join(sorted(unknown)))
    version = re.search(r'const version = "([A-Za-z0-9.-]+)"', (ROOT / "cmd/jand/main.go").read_text()).group(1)
    output = args.out.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    modules = dependencies()
    goroot = run("go", "env", "GOROOT")
    toolchain = run("go", "version")
    produced = []
    with tempfile.TemporaryDirectory(prefix=".jand-build-", dir=output) as temp:
        stage = Path(temp)
        for goos, goarch, label in targets:
            name = f"jand-{version}-{label}-{goarch}"
            folder = stage / name
            folder.mkdir()
            binary = folder / ("jand.exe" if goos == "windows" else "jand")
            environment = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(binary), "./cmd/jand"], cwd=ROOT, env=environment, check=True)
            binary.chmod(0o755)
            signed = notarized = False
            if goos == "darwin" and args.sign:
                sign_macos(binary, args.sign)
                signed = True
                if args.notary_profile:
                    notarize_macos(binary, args.notary_profile)
                    notarized = True
            (folder / "QUICKSTART.md").write_text(quickstart(goos == "windows", args.relay, signed, notarized))
            (folder / "AGENT.md").write_text(agent_doc(goos == "windows", args.relay))
            shutil.copyfile(ROOT / "docs/jand-template.md", folder / "jand-template.md")
            add_licenses(folder, modules, goroot)
            info = {"product": "jand", "version": version, "module": "github.com/jaredchao/jand",
                    "command": binary.name, "goos": goos, "goarch": goarch,
                    "toolchain": toolchain, "cgo_enabled": False,
                    "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                    "dependencies": {k: v["Version"] for k, v in sorted(modules.items())},
                    "platform_signed": signed, "notarized": notarized}
            (folder / "BUILD-INFO.json").write_text(json.dumps(info, ensure_ascii=False, indent=2) + "\n")
            suffix = ".zip" if goos == "windows" else ".tar.gz"
            path = output / (name + suffix)
            archive(folder, path, goos == "windows")
            produced.append(path)
            print(path.name, flush=True)
        kit = stage / f"jand-{version}-relay-deploy"
        kit.mkdir()
        linux = [p for p in produced if "-linux-" in p.name]
        for path in linux:
            shutil.copyfile(path, kit / path.name)
        for source in (ROOT / "deploy").iterdir():
            if source.is_file():
                if source.name == "INSTALL.md":
                    (kit / source.name).write_text(source.read_text().replace("<版本>", version))
                elif source.name in {"jand-relay.supervisor.conf", "nginx.conf.example", "Caddyfile.example", "relay.json.example"}:
                    shutil.copyfile(source, kit / source.name)
        (kit / "SHA256SUMS.txt").write_text("".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in linux))
        deployment = output / (kit.name + ".zip")
        archive(kit, deployment, windows=True)
        produced.append(deployment)
        (output / "SHA256SUMS.txt").write_text("".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in produced))
        (output / "部署与分发说明.md").write_text((ROOT / "deploy/INSTALL.md").read_text().replace("<版本>", version))
    print(f"Created {len(produced)} archives in {output}")


if __name__ == "__main__":
    main()
