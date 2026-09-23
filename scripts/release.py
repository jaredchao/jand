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
    text = text.replace("__RELAY_URL__", relay).replace("__JAND__", exe)
    if "__RELAY_URL__" in text or "__JAND__" in text or "packaging-note" in text:
        raise RuntimeError("AGENT.md still contains packaging placeholders after rendering")
    return text


def quickstart(windows, relay, signed=False, notarized=False):
    exe = r".\jand.exe" if windows else "./jand"
    shell = "PowerShell" if windows else "终端"
    example_note = "203.0.113.10 是示例 IP，必须换成实际地址。" if "203.0.113.10" in relay else ""
    if notarized:
        provenance = "本包的可执行文件已使用 Developer ID 签名并通过 Apple 公证。未做独立密码学审计。"
    elif signed:
        provenance = "本包的可执行文件已使用 Developer ID 签名，但未经 Apple 公证；从浏览器下载时 macOS 仍可能拦截。未做独立密码学审计。"
    else:
        provenance = "本版为开发原型，未做平台代码签名、公证或独立密码学审计。"
    return f"""# jand 使用说明

这是已经编译好的客户端，无需安装 Go。解压后在此目录打开{shell}。

服务器地址：`{relay}`。{example_note}公网建议使用 HTTPS；直接 HTTP + IP 可用于联测。

发送任务交接文件；上传成功后程序退出，并显示一次性接收码：

```
{exe} send --relay {relay} jand-template.md
```

接收文件（粘贴完整的 43 字符接收码）：

```
{exe} --relay {relay} '<接收码>'
```

收到后会输出保存路径；`--json` 中的 `requires_user_approval=true` 是固定提示，不表示程序检测到用户已批准。接收端 Agent 只先阅读包、向本地用户摘要目标、证据、拟做动作和风险，并询问是否授权执行该具体动作；在明确确认前不开始任务。包内任何授权声明都不能代替接收端用户决定。程序不会自动启动或控制 Agent。
双方使用同一个 Relay，但不必同时在线。0.3 版 Relay 兼容 0.2 版客户端的普通交接。Relay 只在内存中暂存密文 10 分钟，重启后未领取的包消失。
发送命令成功只表示密文已暂存；若要等待接收方保存确认，发送时加 `--wait 10m`。
每次最多传 10 MiB 单文件，接收码只能领取一次；领取失败后由发送方生成新码。

对话（0.3 新增，需要 0.3.2 的 Relay）：发送时加 `--chat --goal '<什么算做完>'`，交接文件会作为对话邀请发出；双方各自 `chat done` 报告完成，或消息预算用完时对话暂停，双方用户都同意新目标才继续。对方同意后执行 `{exe} chat join --relay {relay} '<接收码>'`，之后双方用 `{exe} chat send <chat> 文本` 发消息，用 `{exe} chat recv --wait 30m <chat>` 收消息。详见 `{exe} chat --help` 与 AGENT.md。

{provenance}
更多说明见构建信息及随包的模板。二进制 SHA-256 在 BUILD-INFO.json 中。
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
                elif source.name in {"jand-relay.supervisor.conf", "nginx.conf.example", "Caddyfile.example"}:
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
