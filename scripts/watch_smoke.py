#!/usr/bin/env python3
"""Real CLI blocked-Codex regression + temporary Relay; never queues a notice.

Without a verified same-host processing receipt, repeated/concurrent watch starts
must fail closed. Formal recv and ordinary read-only watch remain available.
"""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from smoke import BIN, first_line


def main():
    with tempfile.TemporaryDirectory(prefix="jand-watch-blocked-") as tmp:
        root = Path(tmp)
        host, guest, fake = root / "host", root / "guest", root / "fake-bin"
        fake.mkdir()
        calls = root / "must-not-queue"
        codex = fake / "codex"
        codex.write_text("#!/bin/sh\nprintf 'unexpected queue' >> \"$JAND_WATCH_CALLS\"\n")
        codex.chmod(0o700)
        base_env = dict(os.environ, JAND_PAGE="off", JAND_RELAY="", JAND_ACCESS_TOKEN="", JAND_WATCH_CALLS=str(calls), PATH=str(fake)+os.pathsep+os.environ["PATH"])
        relay = subprocess.Popen([str(BIN), "relay", "--listen", "127.0.0.1:0"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        def cli(home, *args):
            return subprocess.run([str(BIN), *map(str, args)], env=dict(base_env, JAND_HOME=str(home)), capture_output=True, text=True, timeout=12)
        def events(result):
            assert result.returncode == 0, "fixture CLI failed"
            return [json.loads(line) for line in result.stdout.splitlines()]
        def hashes(home):
            return {str(p.relative_to(home)): hashlib.sha256(p.read_bytes()).hexdigest() for p in home.rglob("*") if p.is_file()}
        def blocked(chat):
            result = cli(host, "chat", "watch", "--json", "--agent", "codex", "--thread", "00000000-0000-4000-8000-000000000001", chat)
            assert result.returncode == 1, "unsafe automatic watcher was enabled"
            record = json.loads(result.stdout)
            assert record["event"] == "agent_notify_blocked" and record["state"] == "host_receipt_unavailable"
            assert "recv manually" in record["message"]
        try:
            url = "http://" + re.search(r"\baddr=(\S+)", first_line(relay)).group(1)
            packet = root / "packet.md"; packet.write_text("only fixed local fixture data")
            q = events(cli(host, "send", "--json", "--chat", "--goal", "blocked watch fixture", "--relay", url, packet))[0]
            chat = q["chat"]
            events(cli(guest, "--json", "--relay", url, "--out", root / "received", q["code"]))
            events(cli(guest, "chat", "join", "--json", "--relay", url, q["code"]))
            events(cli(guest, "chat", "send", "--json", "--kind", "request", chat, "must remain available to manual recv"))
            before = hashes(host)
            with ThreadPoolExecutor(max_workers=4) as pool:
                list(pool.map(lambda _: blocked(chat), range(8)))
            assert not calls.exists(), "blocked mode invoked Codex queue"
            assert hashes(host) == before, "blocked watch changed local history or receiver cursor"
            got = events(cli(host, "chat", "recv", "--json", chat))
            assert [e["event"] for e in got] == ["opened", "joined", "message"]
            events(cli(guest, "chat", "send", "--json", "--kind", "request", chat, "later event after a manual receive"))
            # A normal recv is NOT an ack of an unrelated pending host notice.
            blocked(chat)
            assert not calls.exists(), "cursor change incorrectly released automatic queuing"
            got = events(cli(host, "chat", "recv", "--json", chat))
            assert got[0]["id"] == "g2"
            events(cli(guest, "chat", "close", "--json", chat))
            # The non-Agent watcher remains usable and sees the terminal event.
            watch = cli(host, "chat", "watch", "--json", chat)
            assert watch.returncode == 0 and any(e["event"] == "closed" for e in [json.loads(l) for l in watch.stdout.splitlines()])
            print(json.dumps({"real_cli_concurrent_and_restarted_watch":"blocked", "fake_codex_invocations":0, "manual_recv_does_not_release_host_gate":"passed", "unread_events_preserved":"passed", "home_hash_parity":"passed", "ordinary_readonly_watch":"passed", "actual_host_or_user_queue_mutated":False}, indent=2))
        finally:
            relay.terminate(); relay.wait(timeout=5)


if __name__ == "__main__":
    main()
