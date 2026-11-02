#!/usr/bin/env python3
"""Real CLI, temporary authenticated relay, independent homes; no UI or production."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from smoke import BIN, ROOT, first_line


def main():
    with tempfile.TemporaryDirectory(prefix="jand-plugin-smoke-") as tmp:
        root = Path(tmp)
        registry, token = root / "registry.json", root / "alice-token"
        def manage(*args, ok=True):
            p = subprocess.run([sys.executable, str(ROOT / "scripts/relay_tokens.py"), "--registry", str(registry), *map(str, args)], capture_output=True, text=True, timeout=10)
            assert (p.returncode == 0) == ok, "token management returned unexpected status"
            if token.exists():
                assert token.read_text().strip() not in p.stdout + p.stderr, "token leaked in management output"
            return p
        manage("issue", "--subject", "alice", "--token-out", token)
        base = root / "base.json"
        operator = "local-only-operator"
        operator_sha = hashlib.sha256(operator.encode()).hexdigest()
        base.write_text(json.dumps({"listen": "127.0.0.1:0", "access": {"tokens_sha256": [operator_sha]}}))
        candidate = root / "candidate.json"
        manage("export", "--base", base, "--out", candidate)
        # Validate through the actual relay config loader before launch.
        checked = subprocess.run([str(BIN), "relay", "--config", str(candidate), "--print-config"], capture_output=True, text=True)
        assert checked.returncode == 0, "candidate rejected by relay loader"
        relay = None
        def start(config):
            process = subprocess.Popen([str(BIN), "relay", "--config", str(config)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            url = "http://" + re.search(r"\baddr=(\S+)", first_line(process)).group(1)
            return process, url
        def cli(home, *args):
            env = dict(os.environ, JAND_HOME=str(home), JAND_PAGE="update", JAND_RELAY="", JAND_ACCESS_TOKEN="")
            return subprocess.run([str(BIN), *map(str, args)], capture_output=True, text=True, env=env, timeout=15)
        def event(p):
            assert p.returncode == 0, "CLI failed unexpectedly"
            return json.loads(p.stdout.splitlines()[0])
        def snapshot(directory):
            return {str(p.relative_to(directory)): hashlib.sha256(p.read_bytes()).hexdigest() for p in directory.rglob("*") if p.is_file()}
        try:
            relay, url = start(candidate)
            host, guest = root / "host", root / "guest"
            host.mkdir(); guest.mkdir()
            (host / "config.json").write_text(json.dumps({"relay": url, "access_token_file": str(token), "chat": {"page": "update"}}))
            packet = root / "task.md"; packet.write_text("local integration fixture")
            assert cli(guest, "send", "--relay", url, packet).returncode == 1, "unauthorized create accepted"
            q = event(cli(host, "send", "--chat", "--json", "--goal", "local native view", packet))
            chat = q["chat"]
            # Invite receipt and join require only the invitation capability, no access token.
            event(cli(guest, "--json", "--relay", url, "--out", root / "inbox", q["code"]))
            event(cli(guest, "chat", "join", "--json", "--relay", url, q["code"]))
            event(cli(host, "chat", "recv", "--json", chat))
            event(cli(guest, "chat", "send", "--json", "--kind", "request", chat, "native question"))
            before = snapshot(host)
            d = event(cli(host, "chat", "inspect", "--json", chat))
            assert d["schema_version"] == 1 and d["scope"] == "local"
            assert before == snapshot(host), "inspect changed local files"
            got = event(cli(host, "chat", "recv", "--json", chat))
            assert got["kind"] == "request", "inspect consumed pending peer message"
            d = event(cli(host, "chat", "inspect", "--json", chat))
            assert d["messages"][0]["pending"] is True and d["cost"]["integrity"]["state"] == "complete"
            state = json.loads((host / "chats" / (chat + ".json")).read_text())
            exported = json.dumps(d)
            assert state["token"] not in exported and state["key"] not in exported, "protocol secret in inspect"
            # Even offline the snapshot works and is byte-for-byte read-only.
            relay.terminate(); relay.wait(timeout=5); relay = None
            assert event(cli(host, "chat", "inspect", "--json", chat))["chat"]["status"] == "active"
            manage("revoke", "--subject", "alice")
            revoked = root / "revoked.json"
            manage("export", "--base", candidate, "--out", revoked)
            relay, new_url = start(revoked)
            assert cli(host, "send", "--relay", new_url, packet).returncode == 1, "revoked token accepted after candidate restart"
            (guest / "config.json").write_text(json.dumps({"access_token_file": str(root / "operator-token")}))
            (root / "operator-token").write_text(operator)
            assert cli(guest, "send", "--relay", new_url, packet).returncode == 0, "unmanaged operator token lost"
            print(json.dumps({"temporary_authenticated_relay": "passed", "issued_token_create": "passed", "invitation_without_access_token": "passed", "inspect_readonly_and_no_consume": "passed", "inspect_offline_and_no_protocol_secrets": "passed", "revoked_token_denied_after_restart": "passed", "unmanaged_token_preserved": "passed"}, indent=2))
        finally:
            if relay is not None:
                relay.terminate(); relay.wait(timeout=5)


if __name__ == "__main__":
    main()
