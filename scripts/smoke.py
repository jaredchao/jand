#!/usr/bin/env python3
"""Three real CLI processes, no services or files left behind."""
import hashlib
import json
import pathlib
import re
import select
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
BIN = ROOT / "bin" / "jand"


def first_line(process):
    ready, _, _ = select.select([process.stdout], [], [], 10)
    if not ready:
        raise RuntimeError("process did not emit its initial event within 10s")
    line = process.stdout.readline()
    if not line:
        raise RuntimeError("process exited before its initial event")
    return line.strip()


def main():
    processes = []
    try:
        with tempfile.TemporaryDirectory(prefix="jand-smoke-") as tmp:
            root = pathlib.Path(tmp)
            source = root / "task.md"
            source.write_text("# 文档交接\n\n已完成初稿，下一步核对标题。\n", encoding="utf-8")
            relay = subprocess.Popen([str(BIN), "relay", "--listen", "127.0.0.1:0"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, cwd=tmp)
            processes.append(relay)
            # Match the structured log field by name. Counting words breaks
            # whenever the startup line gains a field.
            banner = first_line(relay)
            found = re.search(r"\baddr=(\S+)", banner)
            if not found:
                raise RuntimeError(f"relay did not report a listen address: {banner!r}")
            address = found.group(1)
            url = "http://" + address
            sender = subprocess.run([str(BIN), "send", "--json", "--relay", url, str(source)], capture_output=True, text=True, timeout=15, cwd=tmp)
            assert sender.returncode == 0, sender.stdout + sender.stderr
            queued = json.loads(sender.stdout.splitlines()[0])
            assert queued["event"] == "queued"
            result = subprocess.run([str(BIN), "--json", "--relay", url, "--out", str(root / "inbox"), queued["code"]], capture_output=True, text=True, timeout=15, cwd=tmp)
            assert result.returncode == 0, result.stdout + result.stderr
            events = [json.loads(line) for line in result.stdout.splitlines()]
            saved = next(e for e in events if e["event"] == "saved")
            assert saved["requires_user_approval"] is True
            received = pathlib.Path(saved["path"])
            assert received.read_bytes() == source.read_bytes()
            assert saved["sha256"] == hashlib.sha256(source.read_bytes()).hexdigest()
            replay = subprocess.run([str(BIN), "--json", "--relay", url, queued["code"]], capture_output=True, text=True, timeout=10, cwd=tmp)
            assert replay.returncode == 1, "used code was accepted"
            # Outside the explicit inbox no relay files should have appeared.
            assert {p.name for p in root.iterdir()} == {"task.md", "inbox"}
            print(json.dumps({"three_process_transfer": "passed", "sender_exits_before_receive": "passed", "sha256": "passed", "used_code_rejected": "passed", "relay_working_directory_unchanged": "passed"}, indent=2))
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == "__main__":
    main()
