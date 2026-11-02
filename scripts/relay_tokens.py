#!/usr/bin/env python3
"""Offline per-person relay token management. Never contacts/reloads a relay.

Registry stores hashes only. `issue` writes a new private token file, not stdout.
`export` preserves unmanaged base hashes and writes a NEW candidate configuration.
Revocation takes effect only after an operator deploys the candidate and restarts
relay (which discards in-memory sessions). No registration HTTP API is introduced.
"""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import sys
import tempfile


class TokenError(ValueError):
    """Operator-safe message with no file contents or credential values."""


def now():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def load_registry(path):
    if not path.exists():
        return {"schema_version": 1, "tokens": []}
    data = json.loads(path.read_text(encoding="utf-8"))
    if (not isinstance(data, dict) or set(data) != {"schema_version", "tokens"}
            or type(data.get("schema_version")) is not int or data["schema_version"] != 1
            or not isinstance(data.get("tokens"), list)):
        raise TokenError("unsupported or damaged registry")
    subjects, hashes = set(), set()
    for t in data["tokens"]:
        if (not isinstance(t, dict) or set(t) - {"subject", "sha256", "state", "issued_at", "revoked_at"}
                or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}", t.get("subject", ""))
                or not re.fullmatch(r"[a-f0-9]{64}", t.get("sha256", ""))
                or t.get("state") not in {"active", "revoked"} or not t.get("issued_at")):
            raise TokenError("invalid token registry record")
        if t["sha256"] in hashes or t["subject"] in subjects and t["state"] == "active":
            raise TokenError("duplicate hash or active subject")
        hashes.add(t["sha256"])
        if t["state"] == "active":
            subjects.add(t["subject"])
    return data


def write_new(path, data):
    # Exclusive create avoids overwriting a key or operator configuration.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
    except BaseException:
        path.unlink(missing_ok=True)
        raise


def save_registry(path, data):
    fd, name = tempfile.mkstemp(prefix=".relay-token-registry-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(data, f, indent=2)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)


@contextmanager
def locked(path):
    lock = path.with_name(path.name + ".lock")
    try:
        lock.mkdir(mode=0o700)
    except FileExistsError:
        raise TokenError("registry is locked; verify the other operation ended before removing its lock")
    try:
        yield
    finally:
        lock.rmdir()


def operate(a):
    # Resolve paths once; a token/output must never alias the registry or base.
    registry = a.registry.expanduser().resolve()
    if not registry.parent.is_dir():
        raise TokenError("registry parent must already exist (use a private operator directory)")
    with locked(registry):
        if not registry.exists() and a.action != "issue":
            raise TokenError("registry does not exist; issue the first managed token explicitly")
        data = load_registry(registry)
        if a.action == "issue":
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}", a.subject):
                raise TokenError("subject must be a stable non-secret ID using letters, numbers, . _ @ -")
            if any(t["subject"] == a.subject and t["state"] == "active" for t in data["tokens"]):
                raise TokenError("subject already has an active token; revoke it explicitly first")
            target = a.token_out.expanduser().resolve()
            if target == registry:
                raise TokenError("token path must differ from registry")
            token = secrets.token_urlsafe(32)
            record = {"subject": a.subject, "sha256": hashlib.sha256(token.encode()).hexdigest(),
                      "state": "active", "issued_at": now()}
            write_new(target, token + "\n")
            try:
                data["tokens"].append(record)
                save_registry(registry, data)
            except BaseException:
                target.unlink(missing_ok=True)
                raise
            return {"subject": a.subject, "state": "issued_locally", "production_enabled": False}
        if a.action == "revoke":
            active = [t for t in data["tokens"] if t["subject"] == a.subject and t["state"] == "active"]
            if len(active) != 1:
                raise TokenError("subject has no active managed token")
            active[0].update(state="revoked", revoked_at=now())
            save_registry(registry, data)
            return {"subject": a.subject, "state": "revoked_locally", "production_enabled": False}
        if a.action == "list":
            return {"schema_version": 1, "tokens": [{k: t[k] for k in ("subject", "state", "issued_at", "revoked_at") if k in t} for t in data["tokens"]]}
        if a.action == "export":
            base_path, target = a.base.expanduser().resolve(), a.out.expanduser().resolve()
            if base_path == registry or target in (registry, base_path):
                raise TokenError("candidate output must differ from registry and base")
            base = json.loads(base_path.read_text(encoding="utf-8"))
            if not isinstance(base, dict) or not isinstance(base.get("access", {}), dict):
                raise TokenError("invalid relay base configuration")
            access = base.setdefault("access", {})
            hashes = access.get("tokens_sha256", [])
            if not isinstance(hashes, list) or any(not isinstance(h, str) or not re.fullmatch(r"[A-Fa-f0-9]{64}", h) for h in hashes):
                raise TokenError("invalid base allowlist")
            managed = {t["sha256"] for t in data["tokens"]}
            unmanaged = list(dict.fromkeys(h.lower() for h in hashes if h.lower() not in managed))
            active = [t["sha256"] for t in data["tokens"] if t["state"] == "active"]
            result = unmanaged + active
            # Empty means OPEN in existing Relay semantics: never silently open it.
            if not result:
                raise TokenError("refusing empty allowlist: relay would become public; keep a separate operator token or stop relay")
            access["tokens_sha256"] = result
            write_new(target, json.dumps(base, ensure_ascii=False, indent=2) + "\n")
            return {"state": "candidate_only", "active_managed": len(active), "preserved_unmanaged": len(unmanaged), "restart_required": True, "production_enabled": False}
    raise TokenError("unknown action")


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--registry", type=Path, required=True)
    sub = p.add_subparsers(dest="action", required=True)
    issue = sub.add_parser("issue")
    issue.add_argument("--subject", required=True)
    issue.add_argument("--token-out", type=Path, required=True)
    revoke = sub.add_parser("revoke")
    revoke.add_argument("--subject", required=True)
    sub.add_parser("list")
    export = sub.add_parser("export")
    export.add_argument("--base", type=Path, required=True)
    export.add_argument("--out", type=Path, required=True)
    a = p.parse_args(argv)
    try:
        result = operate(a)
    except TokenError as e:
        print(f"token operation failed: {e}; no production change", file=sys.stderr)
        return 1
    except (OSError, ValueError, TypeError, KeyError):
        # Avoid reflecting arbitrary file content (potential secrets) in errors.
        print("token operation failed: invalid input, conflicting state, locked registry or filesystem failure; no production change", file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
