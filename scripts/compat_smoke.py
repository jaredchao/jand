#!/usr/bin/env python3
"""Old/new client interoperability against the current relay.

Usage: python3 scripts/compat_smoke.py OLD_JAND_BINARY
Build the old binary from a released tag or commit first. Every pairing
(old/new sender x old/new receiver) must transfer and confirm a receipt, and
a new --chat sender must still reach an old receiver as a plain handoff."""
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import tempfile

from smoke import BIN, first_line


def jand(binary, *args):
    result = subprocess.run([str(binary), *args], capture_output=True, text=True, timeout=30)
    return result


def main():
    old = pathlib.Path(sys.argv[1]).resolve()
    new = BIN
    relay = subprocess.Popen([str(new), "relay", "--listen", "127.0.0.1:0"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    report = {"old_client_version": jand(old, "--version").stdout.strip(), "relay_version": jand(new, "--version").stdout.strip()}
    try:
        url = "http://" + re.search(r"\baddr=(\S+)", first_line(relay)).group(1)
        with tempfile.TemporaryDirectory(prefix="jand-compat-") as tmp:
            root = pathlib.Path(tmp)
            packet = root / "task.md"
            packet.write_text("# 兼容性\n\n旧客户端与新 Relay。\n", encoding="utf-8")
            digest = hashlib.sha256(packet.read_bytes()).hexdigest()
            pairs = [("old", old, "old", old), ("old", old, "new", new), ("new", new, "old", old), ("new", new, "new", new)]
            for sname, sender, rname, receiver in pairs:
                # --wait makes the sender verify the receiver's receipt.
                proc = subprocess.Popen([str(sender), "send", "--json", "--wait", "1m", "--relay", url, str(packet)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                queued = json.loads(first_line(proc))
                # 0.2.x cannot parse a code that starts with '-' unless option
                # parsing is ended first; newer receivers handle it themselves.
                end = ["--"] if receiver == old else []
                got = jand(receiver, "--json", "--relay", url, "--out", str(root / f"in-{sname}-{rname}"), *end, queued["code"])
                assert got.returncode == 0, got.stdout + got.stderr
                saved = json.loads(got.stdout.splitlines()[-1])
                assert saved["sha256"] == digest and pathlib.Path(saved["path"]).read_bytes() == packet.read_bytes()
                out, err = proc.communicate(timeout=30)
                assert proc.returncode == 0 and '"delivered"' in out, out + err
                report[f"{sname}_sender_to_{rname}_receiver"] = "passed (delivered)"

            chat = jand(new, "send", "--chat", "--goal", "兼容性检查：旧接收端应按普通交接保存", "--json", "--relay", url, str(packet))
            queued = json.loads(chat.stdout.splitlines()[0])
            got = jand(old, "--json", "--relay", url, "--out", str(root / "in-chat-old"), "--", queued["code"])
            assert got.returncode == 0, got.stdout + got.stderr
            saved = json.loads(got.stdout.splitlines()[-1])
            assert saved["sha256"] == digest
            if report["old_client_version"].startswith(("0.1", "0.2")):
                # Clients before 0.3 know nothing of chats: the invite must degrade to a plain handoff.
                assert "chat_invite" not in saved
                report["new_chat_sender_to_old_receiver"] = "passed (saved as plain handoff)"
            else:
                assert saved.get("chat_invite"), saved
                report["new_chat_sender_to_old_receiver"] = "passed (saved, invite recognised)"
        print(json.dumps(report, indent=2, ensure_ascii=False))
    finally:
        relay.terminate()
        relay.wait(timeout=5)


if __name__ == "__main__":
    main()
