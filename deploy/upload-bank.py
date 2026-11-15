#!/usr/bin/env python3
"""Upload a question bank into the private dataset as bank/.

The bank is the answer key: it contains reference solutions and tests. It must
live in the private dataset (Archnemix/Archx-Eval) and nowhere else -- not in
this public repository, and not in the Space's own repository, which may be
public and whose files the Dockerfile copies into a distributable image.

The relay fetches bank/ from the dataset at boot (BANK_FROM_DATASET=1) and
never publishes it back.

    export HF_TOKEN=hf_...          # token with write on the dataset
    python3 deploy/upload-bank.py relay/testdata/bank

Before trusting the result, confirm the layout is what the relay expects: one
JSONL file per category/difficulty shard, e.g. bank/python/easy.jsonl.
"""

import argparse
import os
import pathlib
import sys

DATASET = "Archnemix/Archx-Eval"
PREFIX = "bank"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("bank_dir", help="local directory holding category/*.jsonl shards")
    ap.add_argument("--dataset", default=DATASET, help=f"target dataset (default {DATASET})")
    ap.add_argument("--prefix", default=PREFIX, help=f"path in repo (default {PREFIX})")
    ap.add_argument("--dry-run", action="store_true", help="validate and list, upload nothing")
    args = ap.parse_args()

    root = pathlib.Path(args.bank_dir)
    if not root.is_dir():
        sys.exit(f"not a directory: {root}")

    shards = sorted(root.rglob("*.jsonl"))
    if not shards:
        sys.exit(f"no .jsonl shards found under {root}; the relay would restore an empty bank")

    print(f"{len(shards)} shard(s) -> {args.dataset}:{args.prefix}")
    for s in shards:
        print(f"  - {s.relative_to(root)}  ({s.stat().st_size} bytes)")

    # A shard with no tests cannot be scored, so catch it before it reaches the
    # Space and silently produces zero-signal results.
    bad = []
    for s in shards:
        text = s.read_text(encoding="utf-8", errors="replace")
        if '"tests"' not in text and '"expected_answer"' not in text and '"rubric"' not in text:
            bad.append(str(s.relative_to(root)))
    if bad:
        print(f"warning: {len(bad)} shard(s) carry no tests/expected_answer/rubric:", file=sys.stderr)
        for b in bad:
            print("  -", b, file=sys.stderr)

    if args.dry_run:
        print("dry run: nothing uploaded")
        return 0

    token = os.environ.get("HF_TOKEN")
    if not token:
        sys.exit("HF_TOKEN is unset; it must have write access to the dataset")

    try:
        from huggingface_hub import HfApi
    except ImportError:
        sys.exit("pip install 'huggingface_hub>=0.24,<1' first")

    api = HfApi(token=token)
    api.upload_folder(
        folder_path=str(root),
        path_in_repo=args.prefix,
        repo_id=args.dataset,
        repo_type="dataset",
        commit_message=f"bank: {len(shards)} shard(s) from {root.name}",
    )
    print(f"uploaded to {args.dataset}:{args.prefix}; restart the Space and look for")
    print("  'bank restored from dataset files=N' in the container log")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
