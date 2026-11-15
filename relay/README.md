# Archx-Eval relay

The API layer that sits between a user's front-end runner and the private
evaluation bank. Written in Go with **zero third-party dependencies**: the
binary links against the standard library only, so builds cannot break on a
dependency, and the container is a few megabytes.

```
runner  ──HTTPS──▶  relay (this service)  ──▶  private HF dataset (bank + state)
                          │
                          └──▶  hashed identity tree + event log
```

## Why these choices

| Decision | Reason |
|---|---|
| Go, stdlib only | goroutine-per-connection concurrency, ~10 ms cold start (HF Spaces sleep), tiny image, no supply-chain surface |
| Marker-set identity, not a composite hash | changing one attribute must not mint a new person |
| Raw values split from hashes | the hashed tree is safe to mirror publicly; raw rows stay private and expire |
| Policy in `internal/core`, transport in `internal/httpapi` | the rules are testable without HTTP |

## Layout

```
relay/
  cmd/relayd/          entrypoint, flags (-healthcheck, -sync, -print-config)
  internal/identity/   client IP resolution, marker building, privacy scoring
  internal/store/      sharded hashed tree, raw tree, event log, results
  internal/bank/       loads and validates the question bank, tracks staleness
  internal/draw/       proportional random selection with no duplicates
  internal/limits/     the 1-hour window and the spam/ban ladder
  internal/reputation/ optional CIDR intelligence (hosting/VPN/Tor)
  internal/hf/         publishes state to the private dataset (or a local dir)
  internal/core/       the decision pipeline that ties it together
  internal/httpapi/    JSON endpoints, error rendering, request logging
  scripts/smoke.sh     end-to-end test against a real HTTP server
  testdata/bank/       small verified fixture bank
```

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health` | readiness + bank inventory (`503` until the bank has stock) |
| `GET` | `/v1/categories` | servable categories |
| `POST` | `/v1/draw` | request up to `DRAW_MAX` examples |
| `POST` | `/v1/feedback` | report runner results back |
| `GET` | `/metrics` | *metrics token* — Prometheus exposition |
| `GET` | `/v1/stale` | *admin* — examples that hit the stale threshold |
| `POST` | `/v1/erase` | *admin* — delete the raw row for an identity |

`POST /v1/draw` requires `Authorization: Bearer $ACCESS_TOKEN` (or
`X-Access-Token`). A relay with no `ACCESS_TOKEN` configured refuses every draw:
it fails closed.

`POST /v1/draw`:

```json
{
  "request_id": "optional-uuid",
  "categories": ["python", "logic"],
  "mix": {"python": 0.7, "logic": 0.3},
  "size": 75,
  "device": {
    "machine_id": "4c4c4544-0044-3510-8054-b4c04f4e4e32",
    "disk_uuid": "9f1b2c3d4e5f60718293a4b5c6d7e8f9",
    "os_family": "linux",
    "arch": "x86_64",
    "runner_version": "0.1.0"
  }
}
```

The response contains **only** what a solver needs — id, category, difficulty,
kind, prompt, language, entrypoint, starter code, tags. Reference solutions,
test bodies, expected answers and explanations are never serialised.

Refusals are uniform: `429` with a `Retry-After` header and
`{"error":{"code":"WINDOW_ACTIVE|SPAM_BURST|SPAM_SUSTAINED|IDENTITY_BANNED", ...}}`,
or `403 PRIVACY_BLOCKED` for masked traffic.

## Identity model

Raw values are **never stored**. Three markers are derived, each hashed with a
salted HMAC-SHA256 truncated to 128 bits, and the originals are discarded
immediately:

| Marker | Source | Changed by |
|---|---|---|
| `ip` | socket address, or the last hop of `X-Forwarded-For` | VPN or a new network |
| `machine_id` | client-reported persistent OS UUID | a fresh VM, or editing a system file |
| `disk_uuid` | client-reported root volume UUID | VM clone or reimage |

**Matching ANY one of them is the same person.** To be treated as new, a caller
must rotate all three. That is the whole point: one cheap change must not reset
the window.

`X-Forwarded-For` is only trusted as far as `TRUSTED_PROXY_HOPS` says, and the
socket address always wins when no proxy is configured. Trusting the full chain
is the classic rate-limit bypass.

There is no `raw/` subtree and no raw-value file of any kind. A stored record
looks like this:

```json
{
  "id": "b3181136b958bbfa08e0e8a5dd93ad57",
  "markers": ["31fff368...", "aa1e58e2...", "3df609f7..."],
  "requests": 2,
  "draws": 1,
  "last_draw": "2026-11-15T16:08:07Z"
}
```

Consequently `POST /v1/erase` has nothing to delete and says so explicitly,
rather than pretending to comply.

## Policy defaults

| Setting | Default | Meaning |
|---|---|---|
| `WINDOW` | `1h` | one granted draw per identity per window |
| `DRAW_MAX` | `75` | hard cap on examples per draw |
| `STALE_AFTER_USES` | `3` | example retires after 3 hand-outs |
| `BURST_COUNT` / `BURST_WINDOW` | `15` / `10s` | burst limit |
| `BURST_BAN` | `2h` | burst penalty |
| `SPAM_COUNT` / `SPAM_WINDOW` | `30` / `60s` | sustained limit |
| `SPAM_BAN` | `48h` | sustained penalty |
| `NET_MAX_DRAWS` / `NET_WINDOW` | `60` / `1h` | per-subnet ceiling |
| `GLOBAL_MAX_DRAWS` / `GLOBAL_WINDOW` | `4000` / `1h` | service-wide ceiling |
| ban escalation | `×2, ×3 …` capped at 14 days | repeat offenders |
| personal-data retention | **none needed** | raw values are never stored |

## Persistence

Two directories hold state, and both must live on persistent storage or a
restart silently forgives every limit:

| Directory | Contents | Survives restart |
|---|---|---|
| `DATA_DIR` | hashed identities, marker index, event log, results | yes, loaded on boot |
| `RATE_DIR` | durable sliding-window request log | yes, replayed on boot |

`RATE_DIR` is deliberately **outside** `DATA_DIR` so it never ends up in the
dataset mirror: it is operational state, not identity data.

The rate log writes each event with a single `append(2)` before answering, so it
survives the process being stopped (the Spaces sleep case), and fsyncs on a
timer so a machine crash loses at most `FLUSH_INTERVAL` of events. Both
behaviours are covered by tests that rebuild the service from disk and assert
the ban still holds.

## Running locally

```bash
cd relay
make test          # unit + race
make smoke         # boots the server against testdata/bank and checks the API
make build         # static binary at bin/relayd
```

```bash
ADDR=:7860 \
BANK_DIR=./testdata/bank \
DATA_DIR=./data \
RELAY_SALT=dev \
ALLOW_LOOPBACK=1 \
./bin/relayd
```

`ALLOW_LOOPBACK=1` exists only for local development; without it loopback and
private addresses are refused, because a private address on a public request
means we cannot rate-limit the caller fairly.

## Security posture

See [`../SECURITY.md`](../SECURITY.md) for the threat model. In short: per-IP,
per-marker and per-subnet limits, a durable ladder that survives restarts,
bearer-token gating on draws, strict header policy, bounded request shapes,
answer material never serialised, and raw identity data kept out of the public
tree.

## Configuration

Every setting is an environment variable — there is no config file, because a
Hugging Face Space provides environment variables and nothing else.

`RELAY_SALT` **must** be set in production and must never change: it keys every
marker hash. `HF_DATA_TOKEN` enables dataset access (bank in, state out). `ADMIN_TOKEN` enables the
operational endpoints. Missing values are reported at startup, and the service
warns loudly when it falls back to the development salt.

## Publishing state to the dataset

The dataset is a **git repository**, so writes are sequential. Every state
change goes through `internal/syncctl`, a single-writer controller that
coalesces changes into batches, keeps exactly one commit in flight, skips
files whose git blob hash is unchanged, and backs off when the remote pushes
back. Queueing is in-memory and bounded, so a slow or broken dataset can never
stall a draw.

Raw identity rows are **never** published: git history is permanent, so a
committed IP could not be deleted after the 30-day retention window.

```bash
./relayd -sync            # export the store tree and push to the private dataset
./relayd -sync -dry-run   # write to SYNC_MIRROR_DIR instead (used by CI)
```

The export is a sharded tree (`identities/`, `markers/`, `raw/`, `events/`,
`results/`), so a lookup is a single file open and any slice can be inspected or
diffed without parsing one giant document.
