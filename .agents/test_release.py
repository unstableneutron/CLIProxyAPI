import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import tarfile
import unittest
import zipfile
from unittest.mock import patch

from release import GO_VERSION, archive_manifest, check_run, inspect, main, next_tag, require_source, run


class ReleaseTests(unittest.TestCase):
    def test_dispatch_rejects_wrong_toolchain_or_unavailable_sysroot(self):
        for version, error in (("go0.0.0", ValueError), ("go" + GO_VERSION, subprocess.CalledProcessError)):
            def fake_run(*args):
                if args[0] == "go":
                    return version
                if args[0] == "bash":
                    raise subprocess.CalledProcessError(22, args)
                self.fail("dispatch must not run with failed preflight")

            with patch("release.inspect", return_value=("abc", "v7.3.6-un.1")), patch("release.run", side_effect=fake_run), patch("sys.argv", ["release.py", "--build"]):
                with self.assertRaises(error):
                    main()

    def test_workflow_and_mise_use_canonical_go_version(self):
        root = Path(__file__).resolve().parent.parent
        workflow = (root / ".github/workflows/release.yaml").read_text()
        self.assertEqual(workflow.count("go-version-file: .go-version"), 3)
        self.assertNotIn("go-version-file: go.mod", workflow)
        self.assertIn('go_version="$(cat .go-version)"', workflow)
        self.assertIn("read_file(path='.go-version')", (root / "mise.toml").read_text())
        self.assertEqual((root / "Dockerfile").read_text().count('GOTOOLCHAIN="go$(cat .go-version)"'), 2)

    def test_first_build_and_historical_tags(self):
        self.assertEqual(next_tag(["v7.3.6"], ["v7.2.94-un.0.1.2"]), "v7.3.6-un.1")

    def test_increment_is_numeric(self):
        self.assertEqual(next_tag(["v7.3.6"], ["v7.3.6-un.2", "v7.3.6-un.10"]), "v7.3.6-un.11")

    def test_new_base_resets(self):
        self.assertEqual(next_tag(["v7.3.7"], ["v7.3.6-un.10"]), "v7.3.7-un.1")

    def test_malformed_tags(self):
        for suffix in ("0", "01", "-1", "1.2", "", "1-extra"):
            with self.subTest(suffix=suffix), self.assertRaises(ValueError):
                next_tag(["v7.3.6"], ["v7.3.6-un." + suffix])

    def test_nonexact_or_ambiguous_base(self):
        for bases in ([], ["v7.3.5", "v7.3.6"]):
            with self.assertRaises(ValueError):
                next_tag(bases, [])

    def test_moved_origin_or_source(self):
        require_source("a", "a", "a")
        for args in (("a", "b"), ("b", "b", "a")):
            with self.assertRaises(ValueError):
                require_source(*args)

    def test_qualification_requires_exact_successful_dispatch(self):
        valid = dict(head_sha="abc", event="workflow_dispatch", path=".github/workflows/release.yaml",
                     conclusion="success", status="completed", display_title="qualify v7.3.6-un.1")
        with patch("release.run", return_value=json.dumps(valid)):
            check_run("123", "abc", "v7.3.6-un.1")
        for field in valid:
            with self.subTest(field=field), patch("release.run", return_value=json.dumps({**valid, field: "wrong"})):
                with self.assertRaises(ValueError):
                    check_run("123", "abc", "v7.3.6-un.1")


class ArchiveTests(unittest.TestCase):
    def test_exact_matrix_provenance_and_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = []
            for system, arch, cgos in (
                ("darwin", "amd64", (0, 1)), ("darwin", "arm64", (0, 1)),
                ("windows", "amd64", (0, 1)), ("windows", "arm64", (0, 1)),
                ("linux", "amd64", (0, 1)), ("linux", "arm64", (0, 1)),
                ("freebsd", "amd64", (1,)), ("freebsd", "arm64", (0,)),
            ):
                for cgo in cgos:
                    ext = "zip" if system == "windows" else "tar.gz"
                    name = f"CLIProxyAPI_7.3.6-un.1_{system}_{'aarch64' if arch == 'arm64' else arch}{'' if cgo else '_no-plugin'}.{ext}"
                    path = Path(directory, name)
                    info = dict(source="abc", tag="v7.3.6-un.1", qualification_run="123", go="go" + GO_VERSION,
                                binary_sha256=hashlib.sha256(b"binary").hexdigest(),
                                settings=dict(GOOS=system, GOARCH=arch, CGO_ENABLED=str(cgo)))
                    files = {"BUILDINFO.json": json.dumps(info).encode(),
                             "cli-proxy-api.exe" if ext == "zip" else "cli-proxy-api": b"binary",
                             **{n: b"doc" for n in ("LICENSE", "README.md", "README_CN.md", "config.example.yaml")}}
                    if ext == "zip":
                        with zipfile.ZipFile(path, "w") as archive:
                            for member, data in files.items():
                                archive.writestr(member, data)
                    else:
                        with tarfile.open(path, "w:gz") as archive:
                            for member, data in files.items():
                                entry = tarfile.TarInfo(member)
                                entry.size = len(data)
                                archive.addfile(entry, io.BytesIO(data))
                    paths.append(path)
            manifest = archive_manifest(directory, "abc", "v7.3.6-un.1", "123")
            self.assertEqual(len(manifest.splitlines()), 14)
            with patch("release.GO_VERSION", "0.0.0"), self.assertRaisesRegex(ValueError, "provenance"):
                archive_manifest(directory, "abc", "v7.3.6-un.1", "123")
            for line, path in zip(manifest.splitlines(), sorted(paths)):
                self.assertEqual(line, f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}")
            for head, tag, run_id in (("wrong", "v7.3.6-un.1", "123"), ("abc", "v7.3.6-un.2", "123"), ("abc", "v7.3.6-un.1", "456")):
                with self.assertRaises(ValueError):
                    archive_manifest(directory, head, tag, run_id)
            Path(directory, "unexpected").touch()
            with self.assertRaisesRegex(ValueError, "exactly"):
                archive_manifest(directory, "abc", "v7.3.6-un.1", "123")
            Path(directory, "unexpected").unlink()
            paths[0].unlink()
            with self.assertRaisesRegex(ValueError, "exactly"):
                archive_manifest(directory, "abc", "v7.3.6-un.1", "123")


class RepositoryTests(unittest.TestCase):
    """Exercise real Git refs; only the GitHub release-list API is stubbed."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        cwd = os.getcwd()
        self.addCleanup(os.chdir, cwd)
        os.chdir(self.temp.name)
        run("git", "init", "--bare", "origin.git")
        run("git", "init", "--bare", "upstream.git")
        run("git", "init", "-b", "main", "source")
        os.chdir("source")
        run("git", "config", "user.email", "test@example.invalid")
        run("git", "config", "user.name", "Test")
        run("git", "commit", "--allow-empty", "-m", "upstream release")
        run("git", "tag", "v7.3.6")
        run("git", "remote", "add", "origin", "../origin.git")
        run("git", "remote", "add", "upstream", "../upstream.git")
        run("git", "push", "upstream", "main", "--tags")
        run("git", "commit", "--allow-empty", "-m", "fork")
        run("git", "push", "origin", "main")
        self.head = run("git", "rev-parse", "HEAD")
        stub = patch("release.run", side_effect=lambda *args: "[[]]" if args[0] == "gh" else run(*args))
        stub.start()
        self.addCleanup(stub.stop)

    def test_exact_base_and_moved_origin(self):
        self.assertEqual(inspect(), (self.head, "v7.3.6-un.1"))
        run("git", "--git-dir=../origin.git", "update-ref", "refs/heads/main", self.head + "^")
        with self.assertRaisesRegex(ValueError, "origin/source moved"):
            inspect(expected=self.head)

    def test_ambiguous_tag_aliases(self):
        run("git", "tag", "v7.3.5", "v7.3.6")
        run("git", "push", "upstream", "refs/tags/v7.3.5")
        with self.assertRaisesRegex(ValueError, "exactly one"):
            inspect()

    def test_nonexact_upstream_base(self):
        run("git", "--git-dir=../upstream.git", "update-ref", "-d", "refs/tags/v7.3.6")
        with self.assertRaisesRegex(ValueError, "exactly one"):
            inspect()

    def test_dirty_and_nonmain(self):
        Path("dirty").touch()
        with self.assertRaisesRegex(ValueError, "clean"):
            inspect()
        Path("dirty").unlink()
        run("git", "checkout", "-b", "other")
        with self.assertRaisesRegex(ValueError, "main"):
            inspect()

    def test_conflicting_local_tag(self):
        run("git", "tag", "v7.3.6-un.1")
        run("git", "push", "origin", "refs/tags/v7.3.6-un.1")
        run("git", "tag", "-f", "v7.3.6-un.1", "HEAD^")
        with self.assertRaisesRegex(ValueError, "tag conflict"):
            inspect()


if __name__ == "__main__":
    unittest.main()
