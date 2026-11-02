#!/usr/bin/env python3
"""Local tests for pinned manifest and offline token operations."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import relay_tokens
import runtime_manifest


class TokenTests(unittest.TestCase):
    def test_issue_revoke_export_preserves_unmanaged_and_never_opens_relay(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            registry, token, base = root / "registry.json", root / "alice-token", root / "base.json"
            def action(name, **kwargs):
                return relay_tokens.operate(argparse.Namespace(registry=registry, action=name, **kwargs))
            with self.assertRaises(relay_tokens.TokenError):
                action("list")
            result = action("issue", subject="alice", token_out=token)
            secret = token.read_text().strip()
            self.assertFalse(result["production_enabled"])
            self.assertEqual(token.stat().st_mode & 0o777, 0o600)
            self.assertEqual(registry.stat().st_mode & 0o777, 0o600)
            self.assertNotIn(secret, registry.read_text() + json.dumps(action("list")))
            with self.assertRaises(relay_tokens.TokenError):
                action("issue", subject="alice", token_out=root / "duplicate")
            unmanaged = hashlib.sha256(b"existing-operator").hexdigest()
            base.write_text(json.dumps({"listen": "127.0.0.1:0", "access": {"tokens_sha256": [unmanaged]}}))
            original = base.read_bytes()
            candidate = root / "candidate.json"
            action("export", base=base, out=candidate)
            hashes = json.loads(candidate.read_text())["access"]["tokens_sha256"]
            self.assertEqual(hashes, [unmanaged, hashlib.sha256(secret.encode()).hexdigest()])
            action("revoke", subject="alice")
            revoked = root / "revoked.json"
            action("export", base=candidate, out=revoked)
            self.assertEqual(json.loads(revoked.read_text())["access"]["tokens_sha256"], [unmanaged])
            self.assertEqual(base.read_bytes(), original)
            with self.assertRaises(relay_tokens.TokenError):
                action("revoke", subject="alice")
            empty = root / "empty.json"
            empty.write_text('{}')
            with self.assertRaises(relay_tokens.TokenError):
                action("export", base=empty, out=root / "must-not-exist")
            self.assertFalse((root / "must-not-exist").exists())
            # Existing files are never overwritten, including revoked secrets.
            with self.assertRaises(FileExistsError):
                action("issue", subject="alice", token_out=token)
            self.assertEqual(token.read_text().strip(), secret)
            self.assertEqual(len(action("list")["tokens"]), 1)
            action("issue", subject="alice", token_out=root / "replacement")
            self.assertEqual(len(action("list")["tokens"]), 2)
            registry.write_text('{bad input containing token')
            with self.assertRaises(ValueError):
                action("export", base=base, out=root / "invalid")


class ManifestTests(unittest.TestCase):
    def test_archive_and_binary_mismatch_are_not_usable(self):
        # Isolate one platform; every branch uses the same digest verification.
        original = runtime_manifest.TARGETS
        runtime_manifest.TARGETS = {"win32-x64": ("windows", "windows", "amd64")}
        try:
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                folder = "jand-1.2.3-windows-amd64"
                name = folder + ".zip"
                binary = b"example binary"
                info = {"version": "1.2.3", "goos": "windows", "goarch": "amd64",
                        "binary_sha256": runtime_manifest.digest(binary), "platform_signed": False, "notarized": False}
                def package():
                    with zipfile.ZipFile(root / name, "w") as z:
                        z.writestr(folder + "/jand.exe", binary)
                        z.writestr(folder + "/BUILD-INFO.json", json.dumps(info))
                    sha = runtime_manifest.digest((root / name).read_bytes())
                    sums = f"{sha}  {name}\n".encode()
                    (root / "SHA256SUMS.txt").write_bytes(sums)
                    return {"tagName": "v1.2.3", "isPrerelease": True,
                            "assets": [{"name": name, "digest": "sha256:" + sha,
                                        "url": "https://github.com/jaredchao/jand/releases/download/v1.2.3/" + name},
                                       {"name": "SHA256SUMS.txt", "digest": "sha256:" + runtime_manifest.digest(sums)}]}
                release = package()
                result = runtime_manifest.build(root, release, "a" * 40)
                self.assertEqual(result["targets"]["win32-x64"]["binary_sha256"], info["binary_sha256"])
                (root / name).write_bytes(b"changed")
                with self.assertRaises(ValueError):
                    runtime_manifest.build(root, release, "a" * 40)
                info["binary_sha256"] = "0" * 64
                release = package()
                with self.assertRaises(ValueError):
                    runtime_manifest.build(root, release, "a" * 40)
        finally:
            runtime_manifest.TARGETS = original


if __name__ == "__main__":
    unittest.main()
