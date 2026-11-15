# Deploying: what to do in each account

Three accounts, three jobs, and **no secrets cross between them except one,
which you paste by hand.**

```
                    ┌─────────────────────────────────────────┐
                    │  GitHub  ·  archnemix/Archx-Eval        │
                    │  PUBLIC repository                      │
                    │  source of truth for: relay code,       │
                    │  deploy scripts, CI                     │
                    │  holds NO Hugging Face token that       │
                    │  can write anything                     │
                    └───────────────┬─────────────────────────┘
                                    │  you copy the code folder
                                    │  (deploy/upload-space.py builds it)
                                    ▼
┌──────────────────────────────────────────────┐
│  Hugging Face · PERDYPTO · archx-eval        │
│  PUBLIC Space  (public on purpose)           │
│                                              │
│  runs the relay. holds one secret:           │
│    HF_DATA_TOKEN  ← paste it in Settings     │
│  every other value is generated here         │
│                                              │
│  disk is ephemeral: nothing survives sleep    │
└───────────────┬──────────────────────────────┘
                │  HTTPS, using HF_DATA_TOKEN
                │  reads bank/ , writes identities/ events/ results/
                ▼
┌──────────────────────────────────────────────┐
│  Hugging Face · Archnemix · Archx-Eval       │
│  PRIVATE dataset                             │
│                                              │
│  bank/        the answer key  (you upload)   │
│  identities/  salted hashes   (relay writes) │
│  markers/     salted hashes   (relay writes) │
│  events/      decisions       (relay writes) │
│  results/     scores          (relay writes) │
└──────────────────────────────────────────────┘

  user's machine                          (built later, step 2)
┌──────────────────────┐
│  front runner        │──HTTPS─▶ Space /v1/draw   (needs ACCESS_TOKEN)
│  pip install …       │◀────────   N questions
└──────────────────────┘
```

**Why the Space is public and the dataset is private.** The Space is only a
doorway: it hands out questions to anyone holding `ACCESS_TOKEN`, and it holds
no answer key and no personal data. The dataset holds the answers, so it is
private and only the Space's `HF_DATA_TOKEN` can open it.

**Why there is no collaborator step.** You create `HF_DATA_TOKEN` **in the
Archnemix account** — the account that already owns the dataset — and paste it
into the Space. A token acts as the account that made it, so it has exactly the
access Archnemix has to its own dataset. Nothing else needs granting, and
PERDYPTO never gets an account-level foothold in Archnemix.

---

# Exactly what to do, in order

Do them in this order. Steps 1–3 are one-time; step 4 repeats on every code
change.

## Step 1 — GitHub (archnemix/Archx-Eval) · no secrets

Nothing to configure. The repository holds no Hugging Face token that can write
the Space or the dataset.

You can delete these leftovers from earlier attempts:

| Where | What | Why it is not needed |
|---|---|---|
| GitHub → Settings → Variables | `HF_SPACE` | no workflow deploys the Space any more |
| GitHub → Settings → Secrets | `HF_TOKEN` | only needed if you want CI to write the dataset directly; you don't |

`.github/workflows/hf-space.yml` has been deleted. `relay.yml` remains and only
runs tests.

## Step 2 — Archnemix · create the dataset token

This is the **only** token in the system that matters, and it must be created by
the account that owns the dataset.

1. Sign in as **Archnemix** → <https://huggingface.co/settings/tokens>
2. **New token** → name it `archx-eval-space` → role **Write**
3. Copy it once; it starts `hf_…`
4. Confirm it works:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
     https://huggingface.co/api/datasets/Archnemix/Archx-Eval | head -c 200
```

A JSON blob with `"id"` means success. `{"error":"Invalid credentials"}` means
the token is wrong or was created under the wrong account.

Keep this value. It becomes `HF_DATA_TOKEN` in step 4.

## Step 3 — Archnemix · put the question bank in the dataset

The bank is the answer key. It goes in the **private dataset**, never in the
public Space and never in the GitHub repo.

Python uploader (recommended — it validates first):

```bash
export HF_DATA_TOKEN=hf_...        # the token from step 2
python3 deploy/upload-bank.py path/to/your/real/bank
```

Or in the browser: open the dataset → **Files → Add file → Upload files** →
create a folder `bank` → upload your `<category>/<difficulty>.jsonl` shards so
the layout is `bank/python/easy.jsonl`, `bank/logic/hard.jsonl`, and so on.

> **The repo's `relay/testdata/bank` is a 14-question toy fixture** (add two
> numbers, reverse a string). It is fine for proving the pipeline, useless as a
> benchmark. Use your real bank here.

Verify: the dataset's file list must show `bank/` with your shards inside it.

## Step 4 — PERDYPTO · configure the Space

### 4a. Build the folder to upload

```bash
python3 deploy/upload-space.py
```

That writes `./space-upload/` containing exactly 22 files:

```
README.md          ← from deploy/space-README.md; its frontmatter selects the docker SDK
Dockerfile
go.mod
cmd/relayd/     main.go  bank.go  sync.go
internal/       apiv1 … all packages the binary imports
```

No bank, no tests, no fixtures. Uploading that folder is the whole deploy.

### 4b. Upload it

1. Open <https://huggingface.co/spaces/PERDYPTO/archx-eval>
2. **Files → Add file → Upload files**
3. Drag the **contents** of `space-upload/` (not the folder itself) into the
   Space root, keeping the `cmd/` and `internal/` folder structure
4. Commit

The Space rebuilds automatically. Watch **Logs → Container**.

**Never upload** a bank, an `.env`, or a token file into the Space. Its
repository is public: a file placed there is a file published.

### 4c. Set the Space secrets

**Settings → Variables and secrets → New secret.**

| Name | Value | Notes |
|---|---|---|
| `HF_DATA_TOKEN` | the `hf_…` from step 2 | created by Archnemix, pasted here by you |
| `RELAY_SALT` | `openssl rand -base64 48` | keys every hash. **Never rotate, never share.** |
| `ACCESS_TOKEN` | `openssl rand -hex 32` | the front runner needs this to draw |
| `ADMIN_TOKEN` | `openssl rand -hex 32` (different) | gates `/v1/stale` and `/v1/erase` |
| `METRICS_TOKEN` | `openssl rand -hex 32` (optional) | gates `/metrics` |

Assign the three generated values from the repo root:

```bash
echo "RELAY_SALT    $(openssl rand -base64 48)"
echo "ACCESS_TOKEN  $(openssl rand -hex 32)"
echo "ADMIN_TOKEN   $(openssl rand -hex 32)"
```

All five go in **Settings → Variables and secrets** as **Secrets**, not public
variables. Do **not** set `ALLOW_LOOPBACK`.

Anything else already listed under secrets in that Space (an old `HF_TOKEN`,
`HF_SPACE`, etc.) can be deleted; nothing reads them.

### 4d. Restart

**Settings → Factory rebuild** (or **Restart**) forces a clean container with
the new secrets. Do this after changing any secret.

## Step 5 — verify it actually works

```bash
SPACE=https://perdypto-archx-eval.hf.space

# 1. awake, bank loaded, dataset sync on
curl -s $SPACE/health
```

Look for `"ready": true` and `"bank": {"_total": <your question count>}`.
Then draw once and once more:

```bash
curl -s -H 'Content-Type: application/json' \
     -H "Authorization: Bearer $ACCESS_TOKEN" \
     -d '{"categories":["python"],"size":3,
          "device":{"machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32"}}' \
     $SPACE/v1/draw
```

The same call again must return **429** `WINDOW_ACTIVE` — that is the rule
working, not an error. In the container log you should see
`bank restored from dataset files=N`, then `bank loaded examples=N`.

Finally confirm the dataset is being written: refresh
`https://huggingface.co/datasets/Archnemix/Archx-Eval/tree/main` and look for
`identities/`, `events/`. Within ~30 s of the draw.

## Step 6 — hand the front runner its two values

When the runner is built it needs only:

| Value | From |
|---|---|
| `SPACE_URL` | `https://perdypto-archx-eval.hf.space` |
| `ACCESS_TOKEN` | the Space secret from step 4c |

Never give it `HF_DATA_TOKEN`, `RELAY_SALT` or `ADMIN_TOKEN`.

---

# Troubleshooting, by log line

| Log line | Meaning | Fix |
|---|---|---|
| `HF_DATA_TOKEN is unset` | no dataset token reached the container | step 4c, then factory rebuild |
| `token is not authorised … (status 403)` | token was not made by the dataset owner, or lacks write | redo step 2 as Archnemix |
| `the dataset holds no bank yet` | `bank/` is missing or in the wrong place | step 3; layout must be `bank/<category>/<difficulty>.jsonl` |
| `bank loaded examples=N` | good | — |
| `the bank is empty: /health will report not-ready` | fetch succeeded but no shards parsed | check the JSONL layout and that each line has `"prompt"` |
| `dataset unreachable; starting with empty state` | outbound HTTPS or the token failed at boot | test the `curl` in step 2 |

# Every day after that

**Changed the code?** `python3 deploy/upload-space.py` → drag → commit.
**Changed a question?** `deploy/upload-bank.py` → restart the Space.
**Nothing changed?** Do nothing. The Space sleeps, wakes, and restores itself.

# Known limitations, stated plainly

- **The Space's disk is ephemeral.** Hugging Face discontinued persistent
  storage for new Spaces, so nothing on that disk survives a restart. Durable
  state is restored from the dataset at boot; only the anti-spam rate ladder
  resets. The one-draw-per-window rule is enforced from the restored identity
  record, so it survives regardless.
- **`hf.CommitSink` has never been run against the real Hugging Face API** from
  this development environment (no outbound network to huggingface.co). It is
  covered by unit tests against fakes and by the boot test described above. The
  first real `identities/` appearing in the dataset is the proof; if it does not
  appear within a minute of a draw, check `/health` → `sync.failures`.
- **One writer.** The dataset is a git repository and accepts one commit at a
  time, so the relay batches writes and serialises them. Do not run two relay
  processes against the same dataset.
