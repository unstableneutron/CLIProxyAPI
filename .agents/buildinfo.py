"""Write allowlisted provenance next to a built release binary."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

binary = Path(sys.argv[1])
metadata = subprocess.check_output(["go", "version", "-m", str(binary)], text=True)
if metadata.splitlines()[0].split()[-1] != "go" + Path(".go-version").read_text().strip():
    raise ValueError("release binary toolchain differs from .go-version")
settings = {}
for line in metadata.splitlines():
    fields = line.strip().split("\t")
    if len(fields) == 2 and fields[0] == "build" and "=" in fields[1]:
        key, value = fields[1].split("=", 1)
        if key in {"GOOS", "GOARCH", "CGO_ENABLED", "GOAMD64", "GOARM64", "-tags"}:
            settings[key] = value
if not {"GOOS", "GOARCH", "CGO_ENABLED"} <= settings.keys():
    raise ValueError("binary lacks target metadata")
info = {
    "source": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
    "tag": os.environ["RELEASE_TAG"],
    "go": metadata.splitlines()[0].split()[-1],
    "settings": settings,
    "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
    "qualification_run": os.environ["GITHUB_RUN_ID"],
}
binary.with_name("BUILDINFO.json").write_text(json.dumps(info, indent=2, sort_keys=True) + "\n")
