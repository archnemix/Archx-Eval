# Deploying the relay to a Hugging Face Space

> **This system spans two different Hugging Face accounts.**
>
> | Thing | Account | URL |
> |---|---|---|
> | Dataset (bank + relay state) | one account | `huggingface.co/datasets/Archnemix/Archx-Eval` |
> | Space (running relay) | **a different account** | `perdypto-archx-eval.hf.space` |
>
> Cross-account access is done with a token, never with shared ownership. The
> Space's account needs its **own** token that can read *and write* the dataset.
> Using the dataset owner's token inside the Space would give the Space's
> runtime the ability to act as that account everywhere, which is more
> authority than it needs.

Everything here is automated by `.github/workflows/hf-space.yml`, which is
triggered by editing `ci/trigger/space.yml`. This document explains what that
workflow uploads, which secrets must exist, and the one-time setup.

---

## One-time setup

### 1. GitHub repository settings

Nothing to add for the Space deploy except one **variable**:

| Kind | Name | Value | Why |
|---|---|---|---|
| Variable | `HF_SPACE` | `Archnemix/Archx-Eval` | target Space repo id |

The `HF_TOKEN` **secret** is already used by the dataset workflows, so it only
needs write access to Spaces as well (see step 3). Nothing secret is ever
committed.

### 2. Create the Space

Do this once, by hand, in the browser:

1. huggingface.co → **New → Space**
2. Name: `Archx-Eval` (organisation `Archnemix`)
3. **SDK: Docker** → *Blank*
4. **Visibility: Private**
5. **Hardware: CPU basic** is enough to start

It does not need a Dockerfile yet: the workflow uploads one.

### 3. Tokens: two of them, with different jobs

There are two independent tokens, and mixing them up is the most likely
deployment mistake.

**A. GitHub secret `HF_TOKEN`** — used by CI to *deploy* the Space. It must be
able to write to the Space repo (`perdypto/Archx-Eval`). It does **not** need
dataset access.

**B. Space secret `HF_TOKEN`** — used by the *running relay* to read and write
the **private dataset** `Archnemix/Archx-Eval`. Because the Space lives in a
different account, this must be a token created **by the Space's account**, and
that account must have been granted access to the dataset.

To grant access, on the dataset page:
**Settings → Permissions / Collaborators → add the Space's account** with
**write** role. Without this the relay starts, serves nothing, and logs
`token is not authorised for Archnemix/Archx-Eval (status 403)`.

Quick check that B works before you trust it — run this locally with the token
you intend to put in the Space:

```bash
curl -s -H "Authorization: Bearer $SPACE_HF_TOKEN" \
     https://huggingface.co/api/datasets/Archnemix/Archx-Eval | head -c 300
```

A JSON blob means the token can read it. `{"error":"Invalid credentials"}` or a
`403` means the collaborator grant is missing.

### 4. Enable persistent storage — still required

The relay now restores its own state from the dataset at boot, so identities and
the event log survive a restart even with no volume. But two things cannot live
in a git dataset and still be correct:

1. **The rate log** must be local and fast; it is written on every request.
2. **Raw identity rows are never published** (see below), so they exist only
   locally.

Without a volume, therefore:

- the spam ladder resets on every restart;
- raw identity rows are lost on every restart, which is survivable but weakens
  the marker set.

Mount persistent storage at `/app/data` for raw rows, and at `/app/rate` (or
`RATE_DIR=/app/data/rate`) for the rate log.

In **Settings → Persistent storage**, mount storage and point it at `/app/data`
(hashed identities, raw rows, event log, results). Ideally mount a second volume
at `/app/rate` for the rate log; if only one volume is available, set
`RATE_DIR=/app/data/rate` so both live on it.

### 5. Set the Space secrets

**Settings → Variables and secrets.** These are read at process start.

#### Required

| Name | Type | Purpose |
|---|---|---|
| `RELAY_SALT` | Secret | **Keys every marker hash.** Generate once with `openssl rand -base64 48`. Never rotate: changing it makes every known user a stranger and resets everyone's window. |
| `ACCESS_TOKEN` | Secret | Required on every `POST /v1/draw`. Without it the relay refuses all draws (it fails closed, never open). Generate with `openssl rand -hex 32`. |
| `ADMIN_TOKEN` | Secret | Gates `/v1/stale` and `/v1/erase`. Different value from `ACCESS_TOKEN`. |
| `HF_TOKEN` | Secret | Lets the relay publish state to the private dataset. Same value as the GitHub secret. |
| `METRICS_TOKEN` | Secret | Gates `/metrics`. Inventory numbers are commercially sensitive, so this endpoint is closed without it. |

#### Optional tuning (sensible defaults are built in)

| Name | Default | Effect |
|---|---|---|
| `WINDOW` | `1h` | one draw per identity per window |
| `DRAW_MAX` | `75` | hard cap per draw |
| `STALE_AFTER_USES` | `3` | example retires after N hand-outs |
| `BURST_COUNT` / `BURST_WINDOW` / `BURST_BAN` | `15` / `10s` / `2h` | burst ladder |
| `SPAM_COUNT` / `SPAM_WINDOW` / `SPAM_BAN` | `30` / `60s` / `48h` | sustained ladder |
| `NET_MAX_DRAWS` / `NET_WINDOW` | `60` / `1h` | per-subnet ceiling |
| `GLOBAL_MAX_DRAWS` / `GLOBAL_WINDOW` | `4000` / `1h` | service-wide ceiling |
| `TRUSTED_PROXY_HOPS` | `1` | how many proxies sit in front of us |
| `MAX_BODY_BYTES` | `1048576` | request body ceiling |
| `FLUSH_INTERVAL` | `5s` | fsync cadence for counters |

#### Must NOT be set in production

`ALLOW_LOOPBACK` — it exists only for local development and disables the refusal
of loopback and private-range clients.

---

## What goes in the dataset, and what must never

The dataset is a **git repository**. Its history is permanent: a file that is
committed and later deleted stays readable in the commit graph for ever. That
single fact drives this table.

| Artefact | Published? | Why |
|---|---|---|
| `identities/<ab>/<id>.json` | **yes** | salted hashes only; safe to keep forever |
| `markers/<ab>/<hash>.json` | **yes** | salted hashes only |
| `events/<date>.jsonl` | **yes** | hashes, country, OS, arch, decision — no raw values |
| `results/<date>.jsonl` | **yes** | benchmark outcomes, which are the point of the system |
| `raw/<ab>/<id>.json` | **NO** | contains the IP address and machine identifier |
| `rate/` | **NO** | operational state, and it changes every second |

The raw rows are the one thing that cannot be honoured in a git repo: publishing
them and then "deleting" them after 30 days would leave the data in history
permanently, which would make the retention promise false. They therefore live
only on the Space's persistent volume, and `Prune` deletes them locally on
schedule. A test (`TestDatasetReceivesNoRawIdentity`) asserts that no published
file ever contains an IP, machine id or disk id.

## Write batching: why the relay does not commit per request

Git accepts one writer at a time. Committing on every request would mean
constant rejected pushes under any real load, and a lost write whenever two
commits raced. The relay therefore queues state changes and publishes them in
batches:

- **Coalesced** — a burst of traffic becomes one commit (`SYNC_INTERVAL`,
  default 30 s).
- **Serialised** — exactly one commit is in flight; a test asserts peak
  concurrency is 1, and it caught a real regression where six commits overlapped.
- **Deduplicated** — a file rewritten many times is queued once, newest wins.
- **Change-detected** — files whose git blob hash already matches the remote are
  skipped, so an idle relay produces no commits at all.
- **Adaptive** — a rate limit or server error doubles the interval up to
  `SYNC_MAX_FILES`/10 min, and success eases it back down.
- **Non-blocking** — queueing is in-memory with a hard budget; if the dataset is
  down the relay keeps serving and counts the drops instead of stalling.

Optional tuning: `SYNC_INTERVAL` (default `30s`), `SYNC_MAX_FILES` (default
`100`), `RESTORE_ON_BOOT` (default `true`). Sync health is visible at `/health`
under `sync` (queued, published, skipped, commits, retries, failures, interval).

## What the deploy uploads

The workflow stages **only** what the Space needs, then commits a log of it to
`ci/logs/space-deploy.txt`. Nothing is uploaded from outside this list:

```
README.md            from deploy/space-README.md (carries the docker sdk frontmatter)
Dockerfile           multi-stage: golang builder -> alpine runtime
go.mod               stdlib only, no dependencies to resolve
cmd/relayd/          entrypoint and the -sync subcommand
internal/**          every package the binary imports
```

**Explicitly excluded:** `testdata/`, `scripts/`, `Makefile`, `.dockerignore`,
`*_test.go`. The test fixtures are not the bank, but there is no reason to ship
them.

**Never uploaded:** the question bank itself. The Space mounts it or fetches it
from the private dataset. If the bank lived in the image, anyone who could pull
the image would have the answers.

## What the Space does on boot

1. Reads configuration from environment variables.
2. Loads the bank from `BANK_DIR`. If it is empty, `/health` returns `503` and
   draws are refused with a clear message rather than serving nothing.
3. Reloads every known identity from `DATA_DIR` and every rate counter from
   `RATE_DIR`, so a restart continues where it left off.
4. Restores `identities/`, `markers/`, `events/` and `results/` from the
   dataset in parallel, so it resumes knowing who it already served.
5. Starts listening on `0.0.0.0:7860` (the port the Space requires).
6. Warns loudly if `RELAY_SALT` is missing or if `HF_TOKEN` is unset.

## Verifying a deployment

```bash
# 1. is it awake and does it see stock?
curl -s https://<space-host>/health

# 2. the flow works end to end
curl -s -H 'Content-Type: application/json' \
     -H "Authorization: Bearer $ACCESS_TOKEN" \
     -d '{"categories":["python"],"size":5,"device":{
           "machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32",
           "disk_uuid":"9f1b2c3d4e5f60718293a4b5c6d7e8f9"}}' \
     https://<space-host>/v1/draw

# 3. the same call again must be refused
#    -> 429 with Retry-After
```

### The one check that must not be skipped

**Confirm the Space sees the real client IP.** Hugging Face proxies requests. If
`TRUSTED_PROXY_HOPS` is wrong, every user appears to come from one address and
the entire per-identity limit collapses into a single shared bucket.

Test it from two different networks (e.g. phone hotspot and home wifi) and
compare the `ip_hash` values in `DATA_DIR/events/<date>.jsonl`. Two different
networks must produce two different hashes. If they do not, adjust
`TRUSTED_PROXY_HOPS` (try `0` and `1`) and re-test.

## Redeploying

```bash
# edit the note, commit, push
vi ci/trigger/space.yml
git commit -am "deploy: reason"
git push
```

The workflow rebuilds from source, uploads, and records the deployment commit in
`ci/outputs/space-deploy.txt`. To roll back, revert the commit and edit the
trigger file again.
