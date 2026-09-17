"""Fail-closed CPA release numbering; invoked through mise."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import zipfile

REPO = "unstableneutron/CLIProxyAPI"
VERSION = r"v[0-9]+\.[0-9]+\.[0-9]+"
GO_VERSION = (Path(__file__).resolve().parent.parent / ".go-version").read_text().strip()


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def next_tag(bases, tags):
    if len(bases) != 1:
        raise ValueError("upstream merge-base must have exactly one stable release tag")
    base = bases[0]
    builds = []
    for tag in tags:
        if "-un" not in tag:
            continue
        # These are the complete historical tags, not a second numbering scheme.
        if tag in {f"v7.2.94-un.0.1.{n}" for n in range(3)}:
            continue
        match = re.fullmatch(rf"({VERSION})-un\.([1-9][0-9]*)", tag)
        if not match:
            raise ValueError(f"malformed fork tag: {tag}")
        if match[1] == base:
            builds.append(int(match[2]))
    return f"{base}-un.{max(builds, default=0) + 1}"


def require_source(head, origin, expected=None):
    if head != origin or (expected is not None and head != expected):
        raise ValueError("source is unpushed or origin/source moved")


def check_run(run_id, head, tag):
    if not re.fullmatch(r"[1-9][0-9]*", run_id):
        raise ValueError("invalid qualification run ID")
    data = json.loads(run("gh", "api", f"repos/{REPO}/actions/runs/{run_id}"))
    if (data["head_sha"] != head or data["event"] != "workflow_dispatch"
            or data["path"] != ".github/workflows/release.yaml"
            or data["conclusion"] != "success" or data["status"] != "completed"
            or data["display_title"] != f"qualify {tag}"):
        raise ValueError("qualification run must succeed for the exact source and tag")


def archive_manifest(directory, head, tag, run_id):
    expected = {}
    for system in ("darwin", "windows", "linux", "freebsd"):
        for arch, asset_arch in (("amd64", "amd64"), ("arm64", "aarch64")):
            for cgo in (0, 1):
                if system == "freebsd" and (arch == "amd64") != bool(cgo):
                    continue
                extension = "zip" if system == "windows" else "tar.gz"
                suffix = "" if cgo else "_no-plugin"
                name = f"CLIProxyAPI_{tag[1:]}_{system}_{asset_arch}{suffix}.{extension}"
                expected[name] = (system, arch, str(cgo))
    paths = {p.name: p for p in Path(directory).iterdir() if p.is_file()}
    if set(paths) != set(expected):
        raise ValueError("qualification directory must contain exactly the 14 release archives")
    manifest = []
    for name in sorted(expected):
        path = paths[name]
        system, arch, cgo = expected[name]
        binary = "cli-proxy-api.exe" if system == "windows" else "cli-proxy-api"
        members = {binary, "BUILDINFO.json", "LICENSE", "README.md", "README_CN.md", "config.example.yaml"}
        if name.endswith(".zip"):
            with zipfile.ZipFile(path) as archive:
                if set(archive.namelist()) != members or len(archive.namelist()) != len(members):
                    raise ValueError(f"unexpected archive members: {name}")
                info = json.loads(archive.read("BUILDINFO.json"))
                digest = hashlib.sha256(archive.read(binary)).hexdigest()
        else:
            with tarfile.open(path) as archive:
                if set(archive.getnames()) != members or len(archive.getmembers()) != len(members):
                    raise ValueError(f"unexpected archive members: {name}")
                if not all(member.isfile() for member in archive.getmembers()):
                    raise ValueError("archive contains non-regular files")
                info = json.load(archive.extractfile("BUILDINFO.json"))
                digest = hashlib.sha256(archive.extractfile(binary).read()).hexdigest()
        if (info["source"] != head or info["tag"] != tag or info["qualification_run"] != run_id
                or info["go"] != "go" + GO_VERSION
                or info["binary_sha256"] != digest
                or info["settings"]["GOOS"] != system or info["settings"]["GOARCH"] != arch
                or info["settings"]["CGO_ENABLED"] != cgo):
            raise ValueError(f"archive provenance mismatch: {name}")
        manifest.append(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {name}\n")
    return "".join(manifest)


def inspect(expected=None):
    if run("git", "branch", "--show-current") != "main":
        raise ValueError("release source must be main")
    if run("git", "status", "--porcelain"):
        raise ValueError("release source must be clean")
    # Dedicated refs avoid trusting stale/local-only tags or conflating remotes.
    run("git", "fetch", "--prune", "--no-tags", "upstream",
        "+refs/heads/main:refs/remotes/upstream/main",
        "+refs/tags/*:refs/release-upstream/*")
    run("git", "fetch", "--prune", "--no-tags", "origin",
        "+refs/heads/main:refs/remotes/origin/main",
        "+refs/tags/*:refs/release-origin/*")
    head = run("git", "rev-parse", "HEAD")
    require_source(head, run("git", "rev-parse", "origin/main"), expected)
    merge_bases = run("git", "merge-base", "--all", "HEAD", "upstream/main").splitlines()
    if len(merge_bases) != 1:
        raise ValueError("ambiguous upstream ancestry")
    bases = []
    for ref in run("git", "for-each-ref", "--format=%(refname)", "refs/release-upstream/").splitlines():
        tag = ref.removeprefix("refs/release-upstream/")
        if re.fullmatch(VERSION, tag) and run("git", "rev-parse", ref + "^{commit}") == merge_bases[0]:
            bases.append(tag)
    tags = [ref.removeprefix("refs/release-origin/") for ref in
            run("git", "for-each-ref", "--format=%(refname)", "refs/release-origin/").splitlines()]
    for existing in tags:
        if "-un" in existing and run("git", "tag", "--list", existing):
            if run("git", "rev-parse", "refs/tags/" + existing) != run("git", "rev-parse", "refs/release-origin/" + existing):
                raise ValueError(f"local/origin tag conflict: {existing}")
    tag = next_tag(bases, tags)
    if run("git", "tag", "--list", tag):
        raise ValueError(f"local tag already exists: {tag}")
    # Listing rather than treating a failed lookup as absence fails closed on API errors.
    releases = json.loads(run("gh", "api", "--paginate", "--slurp", f"repos/{REPO}/releases"))
    if any(release["tag_name"] == tag for page in releases for release in page):
        raise ValueError(f"release already exists: {tag}")
    return head, tag


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--publish", metavar="EXPECTED_TAG")
    parser.add_argument("--build", action="store_true")
    parser.add_argument("--check-run")
    parser.add_argument("--expected-tag")
    args = parser.parse_args()
    if args.check_run:
        check_run(args.check_run, run("git", "rev-parse", "HEAD"), args.expected_tag)
        return
    head, tag = inspect()
    print(f"source={head}\ntag={tag}", flush=True)
    if args.build:
        if run("go", "env", "GOVERSION") != "go" + GO_VERSION:
            raise ValueError("local Go toolchain differs from .go-version")
        run("bash", ".agents/freebsd-sysroot.sh", "--check")
        run("gh", "workflow", "run", "release.yaml", "--repo", REPO, "--ref", "main", "-f", f"tag={tag}")
        return
    if not args.publish:
        return
    if args.publish != tag:
        raise ValueError("requested tag differs from computed tag")
    run_id = os.environ["QUALIFIED_RUN"]
    check_run(run_id, head, tag)
    manifest = archive_manifest(os.environ["QUALIFIED_DIR"], head, tag, run_id)
    subprocess.run(["mise", "run", "verify"], check=True)
    if inspect(expected=head) != (head, tag):
        raise ValueError("release plan changed during verification")
    manifest_hash = hashlib.sha256(manifest.encode()).hexdigest()
    message = f"Release {tag}\n\nQualified-Run: {run_id}\nQualified-Checksums: {manifest_hash}"
    subprocess.run(["git", "tag", "-a", tag, "-m", message], check=True)
    # Atomic update plus an ordinary (non-force) push. Preserve a local tag on
    # failure: a transport error does not prove that the remote rejected it.
    subprocess.run(["git", "push", "--atomic", "origin", f"{head}:main", f"refs/tags/{tag}"], check=True)


if __name__ == "__main__":
    main()
