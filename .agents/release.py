"""Fail-closed CPA release numbering; invoked through mise."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import zipfile

REPO = "unstableneutron/CLIProxyAPI"
VERSION = r"v[0-9]+\.[0-9]+\.[0-9]+"
SNAPSHOT = rf"({VERSION}-dev\.[1-9][0-9]*\.g[0-9a-f]{{12}})-un\.([1-9][0-9]*)"
GO_VERSION = (Path(__file__).resolve().parent.parent / ".go-version").read_text().strip()
# Qualification job target -> (GOOS, GOARCH, CGO modes). Each job publishes its archives.
TARGETS = {
    "darwin-amd64": ("darwin", "amd64", (0, 1)),
    "darwin-arm64": ("darwin", "arm64", (0, 1)),
    "windows-amd64": ("windows", "amd64", (0, 1)),
    "windows-arm64": ("windows", "arm64", (0, 1)),
    "linux-amd64": ("linux", "amd64", (0, 1)),
    "linux-arm64": ("linux", "arm64", (0, 1)),
    "freebsd-amd64": ("freebsd", "amd64", (1,)),
    "freebsd-arm64-no-plugin": ("freebsd", "arm64", (0,)),
}


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
        if match:
            if match[1] == base:
                builds.append(int(match[2]))
        elif not re.fullmatch(SNAPSHOT, tag):
            raise ValueError(f"malformed fork tag: {tag}")
    return f"{base}-un.{max(builds, default=0) + 1}"


def next_snapshot_tag(base, distance, commit, tags):
    if not re.fullmatch(VERSION, base) or distance < 1 or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("invalid upstream dev snapshot")
    snapshot = f"{base}-dev.{distance}.g{commit[:12]}"
    builds = []
    for tag in tags:
        if "-un" not in tag:
            continue
        if tag in {f"v7.2.94-un.0.1.{n}" for n in range(3)}:
            continue
        match = re.fullmatch(SNAPSHOT, tag)
        if match:
            if match[1] == snapshot:
                builds.append(int(match[2]))
        elif not re.fullmatch(rf"({VERSION})-un\.([1-9][0-9]*)", tag):
            raise ValueError(f"malformed fork tag: {tag}")
    return f"{snapshot}-un.{max(builds, default=0) + 1}"


def is_ancestor(ancestor, descendant):
    return subprocess.run(
        ["git", "merge-base", "--is-ancestor", ancestor, descendant],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0


def snapshot_base(target, refs):
    candidates = []
    for ref in refs:
        tag = ref.removeprefix("refs/release-upstream/")
        if not re.fullmatch(VERSION, tag):
            continue
        commit = run("git", "rev-parse", ref + "^{commit}")
        if is_ancestor(commit, target):
            candidates.append((int(run("git", "rev-list", "--count", commit + ".." + target)), tag))
    if not candidates:
        raise ValueError("upstream dev snapshot must have exactly one nearest stable release tag")
    distance = min(item[0] for item in candidates)
    bases = [tag for candidate_distance, tag in candidates if candidate_distance == distance]
    if len(bases) != 1:
        raise ValueError("upstream dev snapshot must have exactly one nearest stable release tag")
    return bases[0], distance


def require_source(head, origin, expected=None):
    if head != origin or (expected is not None and head != expected):
        raise ValueError("source is unpushed or origin/source moved")


def parse_targets(value):
    """Return known targets in canonical order; empty means the full matrix."""
    names = [name.strip() for name in (value or "").split(",") if name.strip()]
    if not names:
        return list(TARGETS)
    if len(set(names)) != len(names) or not set(names) <= TARGETS.keys():
        raise ValueError(f"invalid qualification targets: {value}")
    return [name for name in TARGETS if name in names]


def check_run(run_id, head, tag, targets):
    """Require an exact dispatch whose selected target jobs all succeeded.

    Other targets may fail without blocking; they stay pending and unpublished.
    """
    if not re.fullmatch(r"[1-9][0-9]*", run_id):
        raise ValueError("invalid qualification run ID")
    data = json.loads(run("gh", "api", f"repos/{REPO}/actions/runs/{run_id}"))
    if (data["head_sha"] != head or data["event"] != "workflow_dispatch"
            or data["path"] != ".github/workflows/release.yaml"
            or data["status"] != "completed"
            or data["display_title"] != f"qualify {tag}"):
        raise ValueError("qualification run must complete for the exact source and tag")
    jobs = json.loads(run("gh", "api", f"repos/{REPO}/actions/runs/{run_id}/jobs?per_page=100"))["jobs"]
    conclusions = {job["name"]: job["conclusion"] for job in jobs}
    failed = [target for target in targets if conclusions.get(f"build {target}") != "success"]
    if failed:
        raise ValueError(f"qualification targets did not succeed: {', '.join(failed)}")


def target_archives(tag, target):
    system, arch, cgos = TARGETS[target]
    for cgo in cgos:
        extension = "zip" if system == "windows" else "tar.gz"
        suffix = "" if cgo else "_no-plugin"
        asset_arch = "aarch64" if arch == "arm64" else arch
        yield f"CLIProxyAPI_{tag[1:]}_{system}_{asset_arch}{suffix}.{extension}", (system, arch, str(cgo))


def archive_manifest(directory, head, tag, run_id, targets=tuple(TARGETS)):
    expected = dict(item for target in targets for item in target_archives(tag, target))
    paths = {p.name: p for p in Path(directory).iterdir() if p.is_file()}
    if set(paths) != set(expected):
        raise ValueError("qualification directory must contain exactly the selected targets' release archives")
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
        "+refs/heads/dev:refs/remotes/upstream/dev",
        "+refs/tags/*:refs/release-upstream/*")
    run("git", "fetch", "--prune", "--no-tags", "origin",
        "+refs/heads/main:refs/remotes/origin/main",
        "+refs/tags/*:refs/release-origin/*")
    head = run("git", "rev-parse", "HEAD")
    require_source(head, run("git", "rev-parse", "origin/main"), expected)
    upstream_refs = run("git", "for-each-ref", "--format=%(refname)", "refs/release-upstream/").splitlines()
    tags = [ref.removeprefix("refs/release-origin/") for ref in
            run("git", "for-each-ref", "--format=%(refname)", "refs/release-origin/").splitlines()]
    for existing in tags:
        if "-un" in existing and run("git", "tag", "--list", existing):
            if run("git", "rev-parse", "refs/tags/" + existing) != run("git", "rev-parse", "refs/release-origin/" + existing):
                raise ValueError(f"local/origin tag conflict: {existing}")
    upstream_dev = run("git", "rev-parse", "upstream/dev")
    if is_ancestor(upstream_dev, head):
        base, distance = snapshot_base(upstream_dev, upstream_refs)
        if distance:
            tag = next_snapshot_tag(base, distance, upstream_dev, tags)
        else:
            tag = next_tag([base], tags)
    else:
        merge_bases = run("git", "merge-base", "--all", "HEAD", "upstream/main").splitlines()
        if len(merge_bases) != 1:
            raise ValueError("ambiguous upstream ancestry")
        bases = []
        for ref in upstream_refs:
            tag_name = ref.removeprefix("refs/release-upstream/")
            if re.fullmatch(VERSION, tag_name) and run("git", "rev-parse", ref + "^{commit}") == merge_bases[0]:
                bases.append(tag_name)
        tag = next_tag(bases, tags)
    if run("git", "tag", "--list", tag):
        raise ValueError(f"local tag already exists: {tag}")
    # Listing rather than treating a failed lookup as absence fails closed on API errors.
    releases = json.loads(run("gh", "api", "--paginate", "--slurp", f"repos/{REPO}/releases"))
    if any(release["tag_name"] == tag for page in releases for release in page):
        raise ValueError(f"release already exists: {tag}")
    return head, tag


def tag_message(tag, run_id, manifest_hash, targets):
    pending = [target for target in TARGETS if target not in targets]
    lines = [f"Release {tag}", "", f"Qualified-Run: {run_id}", f"Qualified-Checksums: {manifest_hash}",
             f"Qualified-Targets: {','.join(targets)}"]
    if pending:
        lines.append(f"Pending-Targets: {','.join(pending)}")
    return "\n".join(lines)


def release_status(tag, names):
    """Describe qualification from the exact archive names listed in checksums.txt."""
    names = set(names)
    done = [target for target in TARGETS if all(name in names for name, _ in target_archives(tag, target))]
    pending = [target for target in TARGETS if target not in done]
    if not pending:
        return done, pending, f"Qualification: complete for all targets ({','.join(done)})."
    return done, pending, (f"Qualification: PARTIAL. Qualified and published targets: {','.join(done)}. "
                           f"Pending targets, not qualified and not published: {','.join(pending)}.")


def published_tag(tag):
    """Return the origin tag's commit and its pending targets absent from checksums.txt."""
    run("git", "fetch", "--prune", "--no-tags", "origin", "+refs/tags/*:refs/release-origin/*")
    ref = "refs/release-origin/" + tag
    commit = run("git", "rev-parse", ref + "^{commit}")
    fields = dict(line.split(": ", 1) for line in
                  run("git", "for-each-ref", "--format=%(contents)", ref).splitlines() if ": " in line)
    pending = parse_targets(fields["Pending-Targets"]) if fields.get("Pending-Targets") else []
    # checksums.txt is replaced last, so a target is published only once it is listed there.
    listed = [line.split()[-1] for line in run("gh", "release", "download", tag, "--repo", REPO,
                                               "--pattern", "checksums.txt", "--output", "-").splitlines()]
    return commit, [target for target in pending if target in release_status(tag, listed)[1]]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--publish", metavar="EXPECTED_TAG")
    parser.add_argument("--build", action="store_true")
    parser.add_argument("--build-pending", metavar="TAG")
    parser.add_argument("--complete", metavar="TAG")
    parser.add_argument("--check-run")
    parser.add_argument("--expected-tag")
    parser.add_argument("--archives", metavar="DIR")
    parser.add_argument("--status", metavar="TAG", help="print qualification status for checksums.txt on stdin")
    args = parser.parse_args()
    if args.status:
        print(release_status(args.status, [line.split()[-1] for line in sys.stdin if line.strip()])[2])
        return
    # Unset QUALIFIED_TARGETS selects the full matrix; a subset publishes a
    # partial release whose remaining targets stay explicitly pending.
    targets = parse_targets(os.environ.get("QUALIFIED_TARGETS"))
    if args.check_run:
        head = run("git", "rev-parse", "HEAD")
        check_run(args.check_run, head, args.expected_tag, targets)
        if args.archives:
            print(archive_manifest(args.archives, head, args.expected_tag, args.check_run, targets), end="")
        return
    if args.build_pending or args.complete:
        tag = args.build_pending or args.complete
        commit, pending = published_tag(tag)
        if not pending:
            raise ValueError(f"{tag} has no pending targets")
        if args.build_pending:
            run("gh", "workflow", "run", "release.yaml", "--repo", REPO, "--ref", tag, "-f", f"tag={tag}")
            print(f"source={commit}\ntag={tag}\npending={','.join(pending)}")
            return
        if not os.environ.get("QUALIFIED_TARGETS") or not set(targets) <= set(pending):
            raise ValueError(f"QUALIFIED_TARGETS must name pending targets: {','.join(pending)}")
        run_id = os.environ["QUALIFIED_RUN"]
        check_run(run_id, commit, tag, targets)
        manifest = archive_manifest(os.environ["QUALIFIED_DIR"], commit, tag, run_id, targets)
        manifest_hash = hashlib.sha256(manifest.encode()).hexdigest()
        run("gh", "workflow", "run", "release.yaml", "--repo", REPO, "--ref", tag, "-f", f"tag={tag}",
            "-f", f"complete_run={run_id}", "-f", f"targets={','.join(targets)}", "-f", f"checksums={manifest_hash}")
        print(f"source={commit}\ntag={tag}\ncompleting={','.join(targets)}\nchecksums={manifest_hash}")
        return
    head, tag = inspect()
    print(f"source={head}\ntag={tag}", flush=True)
    if args.build:
        if run("go", "env", "GOVERSION") != "go" + GO_VERSION:
            raise ValueError("local Go toolchain differs from .go-version")
        try:
            run("bash", ".agents/freebsd-sysroot.sh", "--check")
        except subprocess.CalledProcessError:
            # CI verifies the same pinned input independently; only freebsd-amd64 depends on it.
            print("warning: pinned FreeBSD sysroot unavailable locally; freebsd-amd64 may stay pending", flush=True)
        run("gh", "workflow", "run", "release.yaml", "--repo", REPO, "--ref", "main", "-f", f"tag={tag}")
        return
    if not args.publish:
        return
    if args.publish != tag:
        raise ValueError("requested tag differs from computed tag")
    run_id = os.environ["QUALIFIED_RUN"]
    check_run(run_id, head, tag, targets)
    manifest = archive_manifest(os.environ["QUALIFIED_DIR"], head, tag, run_id, targets)
    subprocess.run(["mise", "run", "verify"], check=True)
    if inspect(expected=head) != (head, tag):
        raise ValueError("release plan changed during verification")
    message = tag_message(tag, run_id, hashlib.sha256(manifest.encode()).hexdigest(), targets)
    subprocess.run(["git", "tag", "-a", tag, "-m", message], check=True)
    # Atomic update plus an ordinary (non-force) push. Preserve a local tag on
    # failure: a transport error does not prove that the remote rejected it.
    subprocess.run(["git", "push", "--atomic", "origin", f"{head}:main", f"refs/tags/{tag}"], check=True)


if __name__ == "__main__":
    main()
