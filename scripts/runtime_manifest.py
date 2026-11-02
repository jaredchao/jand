#!/usr/bin/env python3
"""Generate a pinned manifest from release assets. No network or publication.

Requires a `gh release view TAG --json assets,tagName` snapshot, exact tag source
revision and local archives. Verifies each archive against GitHub's asset digest
and the published SHA256SUMS, then verifies the embedded binary against BUILD-INFO.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import tarfile
import zipfile

TARGETS = {"darwin-arm64": ("macos", "darwin", "arm64"),
           "darwin-x64": ("macos", "darwin", "amd64"),
           "win32-x64": ("windows", "windows", "amd64"),
           "linux-arm64": ("linux", "linux", "arm64"),
           "linux-x64": ("linux", "linux", "amd64")}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def build(directory, release, revision):
    tag = release["tagName"]
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?", tag):
        raise ValueError("invalid pinned release tag")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("source revision must be the resolved 40-character tag commit")
    version = tag[1:]
    assets = {a["name"]: a for a in release["assets"]}
    sums_data = (directory / "SHA256SUMS.txt").read_bytes()
    if assets["SHA256SUMS.txt"].get("digest") != "sha256:" + digest(sums_data):
        raise ValueError("published checksum file digest mismatch")
    sums = {}
    for line in sums_data.decode().splitlines():
        value, name = line.split()
        if not re.fullmatch(r"[0-9a-f]{64}", value) or name in sums:
            raise ValueError("invalid or duplicate published checksum")
        sums[name] = value
    targets = {}
    for key, (label, goos, arch) in TARGETS.items():
        folder = f"jand-{version}-{label}-{arch}"
        windows = goos == "windows"
        name = folder + (".zip" if windows else ".tar.gz")
        asset = assets[name]
        data = (directory / name).read_bytes()
        sha = digest(data)
        if sha != sums[name] or asset.get("digest") != "sha256:" + sha:
            raise ValueError(f"archive digest mismatch: {name}")
        expected_url = f"https://github.com/jaredchao/jand/releases/download/{tag}/{name}"
        if asset["url"] != expected_url:
            raise ValueError(f"unexpected asset URL: {name}")
        binary_path = folder + ("/jand.exe" if windows else "/jand")
        info_path = folder + "/BUILD-INFO.json"
        if windows:
            with zipfile.ZipFile(directory / name) as archive:
                names = archive.namelist()
                if names.count(binary_path) != 1 or names.count(info_path) != 1:
                    raise ValueError("missing or duplicate archive members")
                binary = archive.read(binary_path)
                info = json.loads(archive.read(info_path))
        else:
            with tarfile.open(directory / name) as archive:
                for path in (binary_path, info_path):
                    members = [m for m in archive.getmembers() if m.name == path]
                    if len(members) != 1 or not members[0].isfile():
                        raise ValueError("missing, duplicate or non-regular archive member")
                binary = archive.extractfile(binary_path).read()
                info = json.load(archive.extractfile(info_path))
        if (info["version"], info["goos"], info["goarch"], info["binary_sha256"]) != (version, goos, arch, digest(binary)):
            raise ValueError(f"embedded build metadata mismatch: {name}")
        if info.get("source_revision", revision) != revision:
            raise ValueError(f"embedded source revision mismatch: {name}")
        if not isinstance(info["platform_signed"], bool) or not isinstance(info["notarized"], bool):
            raise ValueError("invalid signing metadata")
        targets[key] = {"archive_url": expected_url, "archive_sha256": sha,
                        "archive_format": "zip" if windows else "tar.gz",
                        "binary_path": binary_path, "binary_sha256": digest(binary),
                        "goos": goos, "goarch": arch,
                        "platform_signed": info["platform_signed"], "notarized": info["notarized"],
                        "signature_evidence": "release_build_info"}
    return {"schema_version": 1, "product": "jand", "version": version,
            "release_tag": tag, "source_revision": revision,
            "release_url": f"https://github.com/jaredchao/jand/releases/tag/{tag}",
            "prerelease": release["isPrerelease"], "targets": targets,
            "unsupported_targets": ["win32-arm64"],
            "manifest_signature": "unsigned"}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--assets-dir", type=Path, required=True)
    p.add_argument("--release-json", type=Path, required=True)
    p.add_argument("--source-revision", required=True)
    p.add_argument("--out", type=Path, required=True)
    a = p.parse_args()
    result = build(a.assets_dir, json.loads(a.release_json.read_text()), a.source_revision)
    # Refuse to replace a prior reviewed manifest accidentally.
    with a.out.open("x", encoding="utf-8") as f:
        f.write(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    print(f"manifest written: {a.out}")


if __name__ == "__main__":
    main()
