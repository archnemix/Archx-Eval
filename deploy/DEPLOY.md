# Deploying the relay to a Hugging Face Space

> **This system spans two different Hugging Face accounts.**
>
> | Thing | Account | URL | |
> |---|---|---|---|
> | Dataset (bank + relay state) | **Archnemix** | `huggingface.co/datasets/Archnemix/Archx-Eval` | storage only |
> | Space (running relay) | **PERDYPTO** | `huggingface.co/spaces/PERDYPTO/archx-eval` | runs the code |
> | Front runner (user's machine) | — | not yet built | calls the Space |
>
> Cross-account access is done with a token, never with shared ownership. The
> Space's account needs its **own** token that can read *and write* the dataset.
> Using the dataset owner's token inside the Space would give the Space's
> runtime the ability to act as that account everywhere, which is more
> authority than it needs.

## How the Space gets its code: you upload it, by hand

There is **no automatic deploy**. `.github/workflows/hf-space.yml` was deleted on
purpose: GitHub cannot write to a Space owned by a different account without
holding a token for that account, and putting a PERDYPTO token in the Archnemix
repository's CI is more authority than the automation is worth.

So the split is:

| Account | Receives | How |
|---|---|---|
| **Archnemix** | relay state only (`identities/`, `markers/`, `events/`, `results/`) and the question bank | written by the running relay itself, using its `HF_TOKEN` |
| **PERDYPTO** | the relay's source code | **you upload it manually** (see step 6) |

GitHub Actions therefore needs a token only if you want CI to *write to the
dataset directly*. It does not deploy anything.

---

## One-time setup

### 1. GitHub repository settings

Nothing is *required* for deployment, because deployment is manual. The only
optional item is a **secret** used by CI to prove the dataset is writable:

| Kind | Name | Value | Why |
|---|---|---|---|
| Secret (optional) | `HF_TOKEN` | a token with **write** on `Archnemix/Archx-Eval` | lets CI publish dataset-mirror artefacts |

Delete the old `HF_SPACE` **variable** if you created it: nothing reads it any
more. Nothing secret is ever committed.

### 2. The Space already exists

`huggingface.co/spaces/PERDYPTO/archx-eval` — owned by **PERDYPTO**, which is
*not* the dataset owner. Two consequences that matter:

- The Space's own repository is a **public** artefact unless you set it private.
  Never upload the question bank into it. The bank is fetched from the dataset
  at boot instead (`BANK_FROM_DATASET`, on by default).
- Your Space repo must have **SDK: Docker** and **App port: 7860**. The
  `README.md` frontmatter in `deploy/space-README.md` already declares both.

Set the Space to **Private** while you are testing, then decide deliberately
before making it public.

### 3. Tokens: two of them, with different jobs

There are two independent tokens, and mixing them up is the most likely
deployment mistake.

**A. GitHub secret `HF_TOKEN`** — optional. Used by CI only if you want it to
touch the dataset. It must **not** be able to write the Space, because CI no
longer deploys it.

**B. Space secret `HF_TOKEN`** — used by the *running relay* to read and write
the **private dataset** `Archnemix/Archx-Eval`. Because the Space lives in a
different account, this must be a token created **by the Space's account**, and
that account must have been granted access to the dataset.

To grant access, on the dataset page:
**Settings → Permissions / Collaborators → add `PERDYPTO`** with **write** role.
Without this the relay starts, serves nothing, and logs
`token is not authorised for Archnemix/Archx-Eval (status 403)`.

The relay needs **write**, not just read: it publishes identity hashes, events
and results into the dataset, and that is the mechanism by which its state
survives a Space restart.

Quick check that B works before you trust it — run this locally with the token
you intend to put in the Space:

```bash
curl -s -H "Authorization: Bearer $SPACE_HF_TOKEN" \
     https://huggingface.co/api/datasets/Archnemix/Archx-Eval | head -c 300
```

A JSON blob means the token can read it. `{"error":"Invalid credentials"}` or a
`403` means the collaborator grant is missing.

### 4. Do not count on persistent storage

Hugging Face has **discontinued persistent storage for new Spaces**. If your
Space's settings show no storage section, that is expected, and `/data` is just
another directory on an ephemeral disk. Design accordingly — this system
already does:

| State | Survives a restart because |
|---|---|
| the question bank | fetched from the dataset into `BANK_DIR` at boot |
| identities, markers, events, results | the relay publishes them to the dataset and restores them at boot |
| the rate log (spam ladder) | **nothing** — it is local-only and resets |

So the only consequence of having no volume is that a burst of spam in progress
is forgotten when the Space sleeps. That is an acceptable, documented trade; the
per-identity window is enforced from the restored identity record, not from the
rate log, so **the one-draw-per-window rule survives a restart regardless**.

If you *do* get a volume, mount it at `/app/rate` (or set
`RATE_DIR=/app/data/rate`) and the ladder persists too. Mounting `DATA_DIR` is
still worthwhile: it removes the boot-time restore latency.

### 5. Set the Space secrets

**Settings → Variables and secrets.** These are read at process start.

#### Required

| Name | Type | Purpose |
|---|---|---|
| `RELAY_SALT` | Secret | **Keys every marker hash.** Generate once with `openssl rand -base64 48`. Never rotate: changing it makes every known user a stranger and resets everyone's window. |
| `ACCESS_TOKEN` | Secret | Required on every `POST /v1/draw`. Without it the relay refuses all draws (it fails closed, never open). Generate with `openssl rand -hex 32`. |
| `ADMIN_TOKEN` | Secret | Gates `/v1/stale` and `/v1/erase`. Different value from `ACCESS_TOKEN`. |
| `HF_TOKEN` | Secret | **Created by PERDYPTO**, with write on `Archnemix/Archx-Eval`. Lets the relay fetch the bank and publish state. Not the same value as any GitHub secret unless you deliberately reuse it. |
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
| `BANK_FROM_DATASET` | `true` | fetch the question bank from `bank/` in the dataset at boot |
| `BANK_DIR` | `/app/bank` | where the fetched bank is written |

#### Must NOT be set in production

`ALLOW_LOOPBACK` — it exists only for local development and disables the refusal
of loopback and private-range clients.

---

## What goes in the dataset, and what must never

The dataset is a **git repository**. Its history is permanent: a file that is
committed and later deleted stays readable in the commit graph for ever. That
single fact drives this table.

| Artefact | Direction | Why |
|---|---|---|
| `bank/**/*.jsonl` | **you** -> dataset, read by the relay | the answer key: reference solutions and tests. Written by you, never by the relay, never restored into anything public |
| `identities/<ab>/<id>.json` | relay -> dataset | salted hashes only; safe to keep forever |
| `markers/<ab>/<hash>.json` | relay -> dataset | salted hashes only |
| `events/<date>.jsonl` | **yes** | hashes, country, OS, arch, decision — no raw values |
| `results/<date>.jsonl` | relay -> dataset | benchmark outcomes, which are the point of the system |
| `rate/` | never leaves the Space | operational state, and it changes every second |

There is no `raw/` subtree anywhere in the system, because raw values are never
written down. Publishing personal data into a git repository and then "deleting"
it after 30 days would be a lie: the commit graph keeps it for ever. Rather than
managing that risk, the relay removes it — the IP and device identifiers are
hashed on arrival and discarded, so the dataset, its entire history, and the
Space's volume all contain nothing but hashes.

Asserted by `TestNoRawIdentityAnywhereOnDisk` (sweeps every byte the service
writes), `TestDatasetReceivesNoRawIdentity` (sweeps everything published) and
`TestAccessLogContainsNoRawAddress` (sweeps the HTTP log).

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
2. Restores `identities/`, `markers/`, `events/` and `results/` from the
   dataset (90 s budget), so it resumes knowing who it already served.
3. Fetches `bank/` from the dataset into `BANK_DIR` -- **only if** `BANK_DIR`
   does not already hold `.jsonl` files, so a mounted volume wins. This is the
   step that makes the relay servable at all; without it `/health` returns
   `503` and draws are refused with a clear message.
4. Loads the bank and reloads every rate counter from `RATE_DIR`.
5. Starts listening on `0.0.0.0:7860` (the port the Space requires).
6. Warns loudly if `RELAY_SALT` is missing, if `HF_TOKEN` is unset, or if the
   dataset holds no `bank/` yet.

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
#    -> 429 WINDOW_ACTIVE with Retry-After (seconds until the window clears)
```

The refusal in step 3 is `WINDOW_ACTIVE`, not a ban: one draw per identity per
`WINDOW` (default `1h`) is the normal rule, and `Retry-After` will be close to
`3600`. A ban (`SPAM_BAN`, `BURST_BAN`) only appears after volume abuse.

### The one check that must not be skipped

**Confirm the Space sees the real client IP.** Hugging Face proxies requests. If
`TRUSTED_PROXY_HOPS` is wrong, every user appears to come from one address and
the entire per-identity limit collapses into a single shared bucket.

Test it from two different networks (e.g. phone hotspot and home wifi) and
compare the `ip_hash` values in `DATA_DIR/events/<date>.jsonl`. Two different
networks must produce two different hashes. If they do not, adjust
`TRUSTED_PROXY_HOPS` (try `0` and `1`) and re-test.

## Uploading the code to the Space (manual)

You do this from your machine, with a **PERDYPTO** token. Nothing in this
repository does it for you.

### Option A — the browser (simplest)

1. Open `huggingface.co/spaces/PERDYPTO/archx-eval`.
2. **Files → Add file → Upload files.**
3. Upload so the Space root looks exactly like this:

```
README.md          <- copy deploy/space-README.md to README.md
Dockerfile         <- relay/Dockerfile
go.mod             <- relay/go.mod
cmd/relayd/        <- relay/cmd/relayd/
internal/          <- relay/internal/
```

`README.md` **must** be the one derived from `deploy/space-README.md`, because
its frontmatter is what tells the Space to use the Docker SDK on port 7860.

Do **not** upload `testdata/`, `scripts/`, `Makefile`, `.dockerignore`, any
`*_test.go`, or anything from the dataset. The test fixtures look like a bank
but are placeholders — keeping them out removes any chance of a fake bank being
served, and the Dockerfile copies whatever is present into the image.

### Option B — one command (repeatable)

Save this as `deploy/upload-space.py` in the repo and run it when the code
changes. It stages exactly the same file list as Option A.

```bash
export HF_TOKEN=hf_...     # a PERDYPTO token with write on the Space
python3 deploy/upload-space.py
```

The Space rebuilds its Docker image automatically after every upload. Watch
**Logs → Container**; a healthy boot ends with `listening addr=:7860`.

### After any code change

1. `cd relay && go test ./... -race` — must be green before you ship it.
2. Re-upload with Option A or B.
3. `curl https://perdypto-archx-eval.hf.space/health` and check
   `"ready": true` with a non-zero bank count.

An upload changes only the Space image. The dataset is untouched, so there is
nothing to roll back there.

## Seeding the bank (once, and whenever it changes)

The relay cannot serve without a bank, and the bank is the one thing that must
never sit in the Space repository. Upload it into the dataset as `bank/`:

```bash
export HF_TOKEN=hf_...     # a token with write on Archnemix/Archx-Eval
python3 deploy/upload-bank.py relay/testdata/bank
```

Then restart the Space. The boot log must show
`bank restored from dataset files=N`. If it shows
`the dataset holds no bank yet`, the upload went to the wrong repo or path.

> **The `testdata/bank` fixture is a placeholder, not a benchmark.** It holds 14
> trivial examples (integer addition, longest substring). Seeding it proves the
> plumbing end to end; it does not make the Space a useful evaluator. Replace it
> with the real question bank before pointing the front runner at this Space.

## Two-account summary, in one table

| | Archnemix | PERDYPTO |
|---|---|---|
| holds | the dataset: `bank/`, `identities/`, `markers/`, `events/`, `results/` | the Space: relay code + `README.md` + `Dockerfile` |
| written by | the relay (via its `HF_TOKEN`) and by you for `bank/` | you, manually, on each code change |
| token needed here | `HF_TOKEN` with **write** on the dataset, set as a **Space** secret | none inside the Space; you use a PERDYPTO token locally to upload |
| never put here | — | the question bank, `.env` files, any token |
