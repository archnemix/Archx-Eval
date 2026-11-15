#!/usr/bin/env python3
"""Upload the relay source to the PERDYPTO Space.

The Space belongs to a different Hugging Face account than the dataset, and the
GitHub repository deliberately holds no token for it, so deployment is manual
and explicit: run this, watch the build logs, done.

    export HF_TOKEN=hf_...          # a PERDYPTO token with write on the Space
    python3 deploy/upload-space.py

Staging rules are the whole point of this script: exactly the files the Space
needs, nothing else. In particular the question bank and the test fixtures are
never uploaded, because the Space repository is a public artefact and the bank
is the answer key.
"""

import argparse
import os
import pathlib
import shutil
import sys
import tempfile

SPACE = "PERDYPTO/archx-eval"

# Directories copied wholesale from relay/.
DIRS = ("cmd", "internal")
# Individual files copied from relay/.
FILES = ("Dockerfile", "go.mod")
# README carrying the docker-sdk frontmatter that configures the Space runtime.
README = "deploy/space-README.md"


def stage(repo_root: pathlib.Path) -> pathlib.Path:
    relay = repo_root / "relay"
    out = pathlib.Path(tempfile.mkdtemp(prefix="space-stage-"))

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

    # Defence in depth: test files never ship, even if a directory copy caught
    # one. The Dockerfile builds ./cmd/relayd, so removing them cannot break the
    # image, and it keeps the staging list equal to what the docs promise.
    for p in out.rglob("*_test.go"):
        p.unlink()

    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--space", default=SPACE, help=f"target Space (default {SPACE})")
    ap.add_argument("--dry-run", action="store_true", help="stage and list, upload nothing")
    args = ap.parse_args()

    token = os.environ.get("HF_TOKEN")
    if not token and not args.dry_run:
        sys.exit("HF_TOKEN is unset; it must be a PERDYPTO token with write on the Space")

    repo_root = pathlib.Path(__file__).resolve().parent.parent
    staged = stage(repo_root)

    files = sorted(str(p.relative_to(staged)) for p in staged.rglob("*") if p.is_file())
    print(f"staged {len(files)} file(s) for {args.space}:")
    for f in files:
        print("  -", f)

    # The bank must never appear in this list. Fail loudly rather than upload it.
    leaked = [f for f in files if f.startswith("testdata") or f.startswith("bank")]
    if leaked:
        sys.exit(f"refusing to upload bank or fixture files: {leaked}")

    if args.dry_run:
        print("dry run: nothing uploaded")
        return 0

    try:
        from huggingface_hub import HfApi
    except ImportError:
        sys.exit("pip install 'huggingface_hub>=0.24,<1' first")

    api = HfApi(token=token)
    api.upload_folder(
        folder_path=str(staged),
        repo_id=args.space,
        repo_type="space",
        commit_message="deploy relay",
    )
    print(f"uploaded to {args.space}; watch Logs -> Container for 'listening addr=:7860'")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
