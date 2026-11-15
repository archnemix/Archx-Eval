# Archx-Eval

An evaluation system for AI models and agents, built so that the evaluation
bank cannot be quietly memorised, scraped or trained on.

Three layers, each worked on independently:

| Layer | What it is | Status |
|---|---|---|
| **1. Relay** | API service that validates callers, hardens rate limits and hands out a draw from the private bank | **built** — see [`relay/README.md`](relay/README.md) |
| **2. Front runner** | Runs on the user's device, executes the draw against their model, reports results | not started — needs the live Space |
| **3. Dataset / eval** | Question bank, staleness rotation, rephrasing, benchmark ingesting | not started |

## How the pieces fit

```
 user device                PERDYPTO Space                 Archnemix dataset
┌──────────────┐   HTTPS   ┌──────────────────┐   token   ┌────────────────────┐
│ front runner │ ────────▶ │  relay (Go)      │ ────────▶ │ bank/   (answers)  │
│              │ ◀──────── │  identity+limits │ ◀──────── │         read only  │
│              │  N items  │  draw engine     │           │                    │
└──────┬───────┘           └────────┬─────────┘           │ identities/        │
       │ results                    │ publishes           │ markers/           │
       └──── POST /v1/feedback ─────┘                     │ events/  (hashes)  │
                                                          │ results/           │
                                                          └────────────────────┘
```

The two Hugging Face repos are owned by **different accounts** on purpose: the
Space runs in PERDYPTO, the data lives in Archnemix, and the only bridge is a
token the Space holds with write access to that one dataset.

Nothing is raw. Callers are identified by salted hashes of an IP address or a
device identifier, and the originals are discarded on arrival — so the dataset
holds decision history, never personal data. See [SECURITY.md](SECURITY.md).

## Why build it in this order

The front runner cannot be tested without a real API, and the API can be tested
entirely on its own. So the relay is finished and verified first, then the
runner is built against a contract that already exists.

## Privacy posture

Three values act as identity markers — IP address, machine identifier and disk
identifier — and only because they are the values a user cannot rotate without
real effort. No MAC address, no browser fingerprinting, no cross-service
identifiers, no analytics.

**The raw values are never stored.** They are hashed with a salted HMAC-SHA256
the moment they arrive and discarded. What persists everywhere — disk, dataset,
dataset git history, logs — is 128 bits of hash and nothing else. A full leak of
any of those therefore discloses no personal data, and there is no retention
window to police or deletion to honour. Verified by tests that sweep every file
the service writes.

## CI

`.github/workflows/relay.yml` runs on an edit to `ci/trigger/relay.yml`. Every
log and output is committed back into `ci/` (no artifacts), so results are
auditable from the repository alone.

## Deploying

The Space is deployed **by hand**, not from GitHub:

```bash
export HF_TOKEN=hf_...       # PERDYPTO token with write on the Space
python3 deploy/upload-space.py
```

The question bank is uploaded separately into the private dataset, because it is
the answer key and must never sit in a public repository:

```bash
export HF_TOKEN=hf_...       # token with write on Archnemix/Archx-Eval
python3 deploy/upload-bank.py relay/testdata/bank
```

The relay fetches `bank/` from the dataset at boot, so a Space restart needs no
manual step. Full instructions, including the two-account token setup and the
verification calls, are in [`deploy/DEPLOY.md`](deploy/DEPLOY.md).

Note that Hugging Face has discontinued persistent storage for new Spaces, so
the design assumes an ephemeral disk: durable state is restored from the dataset
on every boot. Only the anti-spam rate log is lost across a restart, and the
one-draw-per-window rule is enforced from the restored identity record, so it
survives regardless.
