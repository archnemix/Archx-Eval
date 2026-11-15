#!/usr/bin/env python3
"""Stage the exact folder to drag onto the Space through the web UI.

You upload the Space by hand — Files -> Add file -> Upload files — so this
script has no credentials and makes no network calls. It just builds a clean
folder with precisely the files the Space needs, so you can drag the contents in
without wondering which ones to include.

    python3 deploy/upload-space.py                  # build ./space-upload
    python3 deploy/upload-space.py --out ~/space    # or somewhere else

Then open the Space, use "Upload files", and drag everything from inside the
output folder into the Space root.

Two things are deliberately never staged:

  * the question bank — it is the answer key and the Space repo is public
  * test fixtures, tests, scripts and the Makefile — not needed to run

The Space fetches `bank/` from the private dataset at boot instead.
"""

import argparse
import pathlib
import shutil
import sys

# Directories copied wholesale from relay/.
DIRS = ("cmd", "internal")
# Individual files copied from relay/.
FILES = ("Dockerfile", "go.mod")
# README carrying the docker-sdk frontmatter that configures the Space runtime.
README = "deploy/space-README.md"


def stage(repo_root: pathlib.Path, out: pathlib.Path) -> pathlib.Path:
    relay = repo_root / "relay"

    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    for d in DIRS:
        src = relay / d
        if not src.is_dir():
            sys.exit(f"missing source directory: {src}")
        shutil.copytree(src, out / d)

    for f in FILES:
        src = relay / f
        if not src.is_file():
            sys.exit(f"missing source file: {src}")
        shutil.copy2(src, out / f)

    shutil.copy2(repo_root / README, out / "README.md")

    # Test files are not needed to build or run, and the Dockerfile copies
    # whatever is present into the image, so strip them.
    for p in out.rglob("*_test.go"):
        p.unlink()

    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default="space-upload", help="output folder (default: ./space-upload)")
    args = ap.parse_args()

    repo_root = pathlib.Path(__file__).resolve().parent.parent
    out = (repo_root / args.out).resolve()
    stage(repo_root, out)

    files = sorted(str(p.relative_to(out)) for p in out.rglob("*") if p.is_file())

    # Fail loudly rather than let the answer key be dragged in by accident.
    leaked = [f for f in files if f.startswith(("testdata", "bank"))]
    if leaked:
        sys.exit(f"refusing to stage bank or fixture files: {leaked}")

    print(f"staged {len(files)} file(s) in {out}")
    for f in files:
        print("  -", f)
    print()
    print("Next: open the Space -> Files -> Add file -> Upload files,")
    print("then drag everything from inside that folder into the Space root.")
    print("README.md must stay named README.md: its frontmatter selects the docker SDK.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
