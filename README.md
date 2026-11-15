# Archx-Eval

An evaluation system for AI models and agents, built so that the evaluation
bank cannot be quietly memorised, scraped or trained on.

Three layers, each worked on independently:

| Layer | What it is | Status |
|---|---|---|
| **1. Relay** | API service that validates callers, hardens rate limits and hands out a draw from the private bank | **built** — see [`relay/README.md`](relay/README.md) |
| **2. Front runner** | Runs on the user's device, executes the draw against their model, reports results | not started |
| **3. Dataset / eval** | Question bank, staleness rotation, rephrasing, benchmark ingesting | not started |

## How the pieces fit

```
 user device                   Hugging Face Space              private HF dataset
┌──────────────┐   HTTPS    ┌──────────────────┐   token    ┌───────────────────┐
│ front runner │ ─────────▶ │  relay (Go)      │ ─────────▶ │ bank/  (questions) │
│              │ ◀───────── │  identity+limits │ ◀───────── │ state/ (hashes)    │
│              │  75 items  │  draw engine     │            │ raw/   (TTL'd)     │
└──────┬───────┘            └──────────────────┘            └───────────────────┘
       │ results
       └──────────────▶ POST /v1/feedback ──▶ results/ (benchmark data, both ways)
```

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
