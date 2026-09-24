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
            waiting = subprocess.Popen([str(BIN), "chat", "recv", "--json", "--wait", "2m", "--wake", "request", chat], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)
            events(jand(host, "chat", "send", "--json", "--kind", "progress", chat, "前端骨架已完成"))
            time.sleep(1.5)
            assert waiting.poll() is None, "progress woke a recv that waits for requests"
            sent_at = time.monotonic()
            events(jand(host, "chat", "send", "--json", "--kind", "request", chat, "GET /users", "返回什么？"))
            out, err = waiting.communicate(timeout=10)
            woke = time.monotonic() - sent_at
            assert waiting.returncode == 0, out + err
            held, message = [json.loads(line) for line in out.splitlines()]
            assert held["kind"] == "progress" and held["id"] == "h1", held
            assert message["event"] == "message" and message["kind"] == "request" and message["id"] == "h2"
            assert message["text"] == "GET /users 返回什么？" and message["untrusted"] is True

            reply = subprocess.run([str(BIN), "chat", "send", "--json", "--kind", "reply", "--reply-to", "h2", chat, "-"], input="[{\"id\":1}]\n", capture_output=True, text=True, timeout=15, env=env)
            events(reply)
            got = events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["text"] == '[{"id":1}]' and got[0]["reply_to"] == "h2", got

            # A delivery written without reading the peer's delivery is refused (exit 5).
            events(jand(guest, "chat", "send", "--json", "--kind", "delivery", chat, "schema v1"))
            blind = jand(host, "chat", "send", "--kind", "delivery", chat, "页面 v1")
            assert blind.returncode == 5 and "unread" in blind.stderr, blind
            got = events(jand(host, "chat", "recv", "--json", chat))
            assert got[0]["id"] == "g2", got
            events(jand(guest, "chat", "send", "--json", "--kind", "delivery", "--supersedes", "g2", chat, "schema v2"))
            events(jand(host, "chat", "recv", "--json", chat))
            stale = jand(host, "chat", "send", "--kind", "reply", "--reply-to", "g2", chat, "v1 可以")
            assert stale.returncode == 1 and "superseded by g3" in stale.stderr, stale
            events(jand(host, "chat", "send", "--json", "--kind", "reply", "--reply-to", "g3", chat, "v2 可以"))
            events(jand(guest, "chat", "recv", "--json", chat))

            # Each side reports its share done; the relay pauses once both have.
            events(jand(guest, "chat", "done", "--json", "--summary", "后端完成", chat))
            got = events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["event"] == "done" and got[0]["text"] == "后端完成", got
            events(jand(host, "chat", "done", "--json", chat))
            for side in (host, guest):
                got = events(jand(side, "chat", "recv", "--json", "--wait", "5s", chat))
                assert got[-1]["event"] == "checkpoint" and got[-1]["reason"] == "all_done", got
                assert "paused, not ended" in got[-1]["message"], got
            # A closing note still passes the pause, marked as such.
            events(jand(guest, "chat", "send", "--json", chat, "收尾：以 g3 为准"))
            got = events(jand(host, "chat", "recv", "--json", chat))
            assert got[0]["after_pause"] is True, got
            events(jand(guest, "chat", "propose", "--json", "--goal", "补 /users 分页", chat))
            events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat))
            events(jand(host, "chat", "accept", "--json", chat))
            for side in (host, guest):
                events(jand(side, "chat", "recv", "--json", "--wait", "5s", chat))

            # Goal reached: pause, the guest proposes the next goal, the host accepts.
            events(jand(host, "chat", "checkpoint", "--json", "--summary", "字段已对齐", chat))
            got = events(jand(guest, "chat", "recv", "--json", "--wait", "5s", chat))
            assert got[0]["event"] == "checkpoint" and got[0]["text"] == "字段已对齐", got
            paused = jand(guest, "chat", "send", "--kind", "request", chat, "还能说吗")
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

            # A workflow from the repository: review, where the implementer's done pauses at once.
            review = pathlib.Path(__file__).resolve().parents[1] / "workflows" / "review.json"
            queued = events(jand(host, "send", "--chat", "--workflow", str(review), "--goal", "按 review 流程交付", "--json", "--relay", url, str(packet)))[0]
            chat2 = queued["chat"]
            saved = events(jand(guest, "--json", "--relay", url, "--out", str(root / "inbox"), queued["code"]))[-1]
            assert saved["workflow"]["name"] == "review" and saved["workflow"]["done_rule"] == "any" and saved["budget"] == 30, saved
            joined = events(jand(guest, "chat", "join", "--json", "--relay", url, queued["code"]))[0]
            assert joined["workflow"]["name"] == "review"
            refused = jand(guest, "chat", "send", chat2, "随便聊聊")
            assert refused.returncode == 1 and "does not allow note" in refused.stderr, refused
            events(jand(guest, "chat", "send", "--json", "--kind", "delivery", chat2, "实现在 src/"))
            events(jand(guest, "chat", "done", "--json", "--summary", "请验收", chat2))
            got = events(jand(host, "chat", "recv", "--json", "--wait", "5s", chat2))
            assert [e["event"] for e in got][-1] == "checkpoint" and got[-1]["reason"] == "any_done", got

            transcript = (host / "chats" / f"{chat}.transcript.jsonl").read_text(encoding="utf-8").splitlines()
            kinds = [json.loads(line)["kind"] for line in transcript]
            assert kinds == ["message", "message", "message", "message", "message", "message", "done", "done", "checkpoint",
                             "message", "proposal", "resumed",
                             "checkpoint", "proposal", "resumed", "closed"], kinds
            print(json.dumps({"invite_and_join": "passed", "background_recv_woken_by_peer": f"passed ({woke:.2f}s)",
                              "progress_held_until_request": "passed", "reply_to": "passed", "both_done_pauses": "passed", "unread_blocks_exit_5": "passed", "supersedes": "passed", "closing_note_after_pause": "passed", "workflow_review_any_done": "passed",
                              "stdin_message": "passed", "checkpoint_propose_accept": "passed",
                              "close_and_exit_code_4": "passed", "transcript": "passed"}, indent=2))
    finally:
        if relay is not None:
            relay.terminate()
            relay.wait(timeout=5)


if __name__ == "__main__":
    main()
