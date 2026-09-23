#!/usr/bin/env python3
"""Chat over real CLI processes: invite, join, a blocked recv woken by the
peer's message, reply, close. Each side has its own JAND_HOME.

Usage: chat_smoke.py [RELAY_URL]; without a URL a local relay is started."""
import json
import os
import pathlib
import re
import subprocess
import sys
import tempfile
import time

from smoke import BIN, first_line


def jand(home, *args, timeout=15):
    env = dict(os.environ, JAND_HOME=str(home))
    env.pop("JAND_RELAY", None)
    return subprocess.run([str(BIN), *args], capture_output=True, text=True, timeout=timeout, env=env)


def events(result):
    assert result.returncode == 0, result.stdout + result.stderr
    return [json.loads(line) for line in result.stdout.splitlines()]


def main():
    relay = None
    if len(sys.argv) < 2:
        relay = subprocess.Popen([str(BIN), "relay", "--listen", "127.0.0.1:0"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        url = sys.argv[1] if relay is None else "http://" + re.search(r"\baddr=(\S+)", first_line(relay)).group(1)
        with tempfile.TemporaryDirectory(prefix="jand-chat-smoke-") as tmp:
            root = pathlib.Path(tmp)
            host, guest = root / "host", root / "guest"
            packet = root / "handoff.md"
            packet.write_text("# 联调\n\n对一下 /users 接口。\n", encoding="utf-8")

            queued = events(jand(host, "send", "--chat", "--goal", "对齐 /users 响应字段", "--budget", "10", "--json", "--relay", url, str(packet)))[0]
            chat = queued["chat"]
            saved = events(jand(guest, "--json", "--relay", url, "--out", str(root / "inbox"), queued["code"]))[-1]
            assert saved["chat_invite"] is True and "chat" not in saved and saved["goal"] == "对齐 /users 响应字段" and saved["budget"] == 10

            assert [e["event"] for e in events(jand(host, "chat", "recv", "--json", chat[:8]))] == ["opened"]
            joined = events(jand(guest, "chat", "join", "--json", "--relay", url, queued["code"]))[0]
            assert joined["event"] == "joined"
            assert [e["event"] for e in events(jand(host, "chat", "recv", "--json", chat))] == ["joined"]

            # The pattern an agent host uses: a background recv that exits
            # when the peer speaks.
            env = dict(os.environ, JAND_HOME=str(guest))
            waiting = subprocess.Popen([str(BIN), "chat", "recv", "--json", "--wait", "2m", chat], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)
            time.sleep(1.5)
            assert waiting.poll() is None, "recv returned before any message"
            sent_at = time.monotonic()
            events(jand(host, "chat", "send", "--json", chat, "GET /users", "返回什么？"))
            out, err = waiting.communicate(timeout=10)
            woke = time.monotonic() - sent_at
            assert waiting.returncode == 0, out + err
            message = json.loads(out.splitlines()[0])
            assert message["event"] == "message" and message["text"] == "GET /users 返回什么？" and message["untrusted"] is True

            reply = subprocess.run([str(BIN), "chat", "send", "--json", chat, "-"], input="[{\"id\":1}]\n", capture_output=True, text=True, timeout=15, env=env)
            events(reply)
            got = events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["text"] == '[{"id":1}]', got

            # Goal reached: pause, the guest proposes the next goal, the host accepts.
            events(jand(host, "chat", "checkpoint", "--json", "--summary", "字段已对齐", chat))
            got = events(jand(guest, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["event"] == "checkpoint" and got[0]["text"] == "字段已对齐", got
            paused = jand(guest, "chat", "send", chat, "还能说吗")
            assert paused.returncode == 1 and "paused" in paused.stderr, paused
            events(jand(guest, "chat", "propose", "--json", "--goal", "再对 /orders", "--budget", "5", chat))
            got = events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["event"] == "proposal" and got[0]["goal"] == "再对 /orders" and got[0]["budget"] == 5, got
            events(jand(host, "chat", "accept", "--json", chat))
            for side in (host, guest):
                got = events(jand(side, "chat", "recv", "--json", "--wait", "5s", chat))
                assert got[0]["event"] == "resumed" and got[0]["goal"] == "再对 /orders", got

            events(jand(guest, "chat", "close", "--json", chat))
            got = events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["event"] == "closed" and got[0]["by"] == "peer"
            late = jand(host, "chat", "send", chat, "还在吗")
            assert late.returncode == 4, late

            transcript = (host / "chats" / f"{chat}.transcript.jsonl").read_text(encoding="utf-8").splitlines()
            assert len(transcript) == 6, transcript
            print(json.dumps({"invite_and_join": "passed", "background_recv_woken_by_peer": f"passed ({woke:.2f}s)",
                              "stdin_message": "passed", "checkpoint_propose_accept": "passed",
                              "close_and_exit_code_4": "passed", "transcript": "passed"}, indent=2))
    finally:
        if relay is not None:
            relay.terminate()
            relay.wait(timeout=5)


if __name__ == "__main__":
    main()
