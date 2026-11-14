# Deploying the relay to a Hugging Face Space

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

### 3. Check the token's scope

`HF_TOKEN` must have **write** access. It is used for two different things:

| Use | Needs |
|---|---|
| pushing to the private dataset | write on that dataset |
| pushing to the Space | write on Spaces |

A *fine-grained* token with both repos scoped in is better than a broad one. To
verify it works before deploying, edit `ci/trigger/space.yml` and watch the
`hf-space` run: the "Verify secrets" step fails fast and prints which value is
missing (never the value itself).

### 4. Enable persistent storage — **do this, or nothing survives**

A Space restarts constantly. Without persistent storage:

- every remembered identity is forgotten, so every user gets a fresh 1-hour
  window on every restart;
- the rate log is emptied, so the spam ladder resets;
- the bank usage counters reset, so nothing ever goes stale.

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
4. Starts listening on `0.0.0.0:7860` (the port the Space requires).
5. Warns loudly if `RELAY_SALT` is missing or if `HF_TOKEN` is unset.

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
