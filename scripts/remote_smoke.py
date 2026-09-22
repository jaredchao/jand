#!/usr/bin/env python3
"""Exercise the public relay path with a disposable file and no printed code."""

import argparse
import hashlib
import json
from pathlib import Path
import select
import subprocess
import tempfile
from urllib.parse import urlsplit
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[1]
BIN = ROOT / "bin" / "jand"


class SmokeFailure(Exception):
    pass


def events(output):
    try:
        return [json.loads(line) for line in output.splitlines() if line]
    except json.JSONDecodeError as exc:
        raise SmokeFailure("invalid CLI JSON output") from exc


def event(items, name):
    return next((item for item in items if item.get("event") == name), None)


def run_client(stage, args, timeout):
    try:
        return subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise SmokeFailure(f"{stage}: timed out") from exc


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--relay", required=True, help="public HTTPS relay origin")
    parser.add_argument("--allow-http-loopback", action="store_true", help="only for local test")
    args = parser.parse_args()
    url = urlsplit(args.relay)
    loopback = url.hostname in {"127.0.0.1", "localhost", "::1"}
    if (url.scheme != "https" and not (args.allow_http_loopback and url.scheme == "http" and loopback)) or not url.hostname or url.username or url.password or url.path not in {"", "/"} or url.query or url.fragment:
        parser.error("--relay must be an HTTPS origin (HTTP allowed only for loopback test)")
    if not BIN.is_file():
        parser.error("build bin/jand first with make build")

    try:
        with urlopen(args.relay.rstrip("/") + "/healthz", timeout=15) as response:
            if response.status != 200:
                raise SmokeFailure(f"health/transport: HTTP {response.status}")
    except OSError as exc:
        raise SmokeFailure(f"health/transport: {type(exc).__name__}") from exc

    with tempfile.TemporaryDirectory(prefix="jand-remote-smoke-") as tmp:
        root = Path(tmp)
        source = root / "smoke.md"
        source.write_text("# jand smoke\n\n一次性公开入口收发测试。\n", encoding="utf-8")
        digest = hashlib.sha256(source.read_bytes()).hexdigest()
        sender = subprocess.Popen([str(BIN), "send", "--json", "--wait", "30s", "--relay", args.relay, str(source)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            ready, _, _ = select.select([sender.stdout], [], [], 30)
            if not ready:
                raise SmokeFailure("upload: no queued event within 30 seconds")
            queued = event(events(sender.stdout.readline()), "queued")
            if not queued or not queued.get("code"):
                raise SmokeFailure("upload: missing queued code")
            code = queued["code"]
            receiver = run_client("receive", [str(BIN), "--json", "--relay", args.relay, "--out", str(root / "inbox"), code], 30)
            if receiver.returncode != 0:
                raise SmokeFailure(f"receive: exit {receiver.returncode}")
            saved = event(events(receiver.stdout), "saved")
            if not saved or not saved.get("path") or saved.get("sha256") != digest:
                raise SmokeFailure("receive: saved bytes or SHA-256 differ")
            try:
                saved_bytes = Path(saved["path"]).read_bytes()
            except OSError as exc:
                raise SmokeFailure("receive: saved file is missing or unreadable") from exc
            if saved_bytes != source.read_bytes():
                raise SmokeFailure("receive: saved bytes or SHA-256 differ")
            try:
                remaining, _ = sender.communicate(timeout=40)
            except subprocess.TimeoutExpired as exc:
                raise SmokeFailure("receipt: timed out") from exc
            if sender.returncode != 0 or not event(events(remaining), "delivered"):
                raise SmokeFailure(f"receipt: no verified delivered event, exit {sender.returncode}")
            replay = run_client("replay", [str(BIN), "--json", "--relay", args.relay, code], 20)
            rejected = event(events(replay.stdout), "error")
            if replay.returncode != 1 or not rejected or "unavailable, expired or already claimed" not in rejected.get("message", ""):
                raise SmokeFailure("replay: used code was not conclusively rejected")
        finally:
            if sender.poll() is None:
                sender.terminate()
                try:
                    sender.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    sender.kill()
                    sender.wait(timeout=5)
    print(json.dumps({"transport": "https" if url.scheme == "https" else "http-loopback", "health": "passed", "queued": "passed", "saved_sha256": "passed", "delivered_receipt": "passed", "used_code_rejected": "passed"}))


if __name__ == "__main__":
    try:
        main()
    except SmokeFailure as exc:
        raise SystemExit(f"remote smoke failed: {exc}") from exc
