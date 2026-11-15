---
title: Archx-Eval Relay
emoji: 🛡️
colorFrom: indigo
colorTo: gray
sdk: docker
app_port: 7860
pinned: false
license: mit
short_description: Hardened draw API for the Archx-Eval question bank
---

# Archx-Eval Relay

The API layer between a user's front-end runner and the private Archx-Eval
question bank.

The source of this Space is
[`archnemix/Archx-Eval`](https://github.com/archnemix/Archx-Eval). The folder
built by `deploy/upload-space.py` is uploaded here by hand through the web UI;
GitHub does not deploy it and holds no token for this account.

This Space is public and holds no answer key and no personal data. The private
dataset does, and the only key to it is `HF_DATA_TOKEN`.

The question bank is **not** in this repository. It is fetched at boot from the
private dataset `Archnemix/Archx-Eval`, because the answer key must never sit in
an artefact that can be pulled.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/health` | none | readiness and bank inventory |
| `GET` | `/v1/categories` | none | servable categories |
| `GET` | `/metrics` | metrics token | Prometheus exposition |
| `POST` | `/v1/draw` | access token | request up to 75 examples |
| `POST` | `/v1/feedback` | none | report runner results |
| `GET` | `/v1/stale` | admin token | examples at the use limit |
| `POST` | `/v1/erase` | admin token | reports that there is nothing to delete — only hashes are stored |

## Required secrets

Set these in **Settings → Variables and secrets** for this Space:

| Name | Purpose |
|---|---|
| `RELAY_SALT` | keys every marker hash. Set once, never change it. |
| `ACCESS_TOKEN` | required on every draw |
| `ADMIN_TOKEN` | gates the operational endpoints |
| `HF_DATA_TOKEN` | **created by the dataset owner (Archnemix)** with write access to its own dataset `Archnemix/Archx-Eval`, then pasted here. Fetches `bank/` and publishes relay state. No collaborator grant is needed: the token acts as the account that made it. |
| `METRICS_TOKEN` | optional; gates `/metrics` |

`BANK_FROM_DATASET` (default `true`) fetches `bank/` from the dataset at boot;
set it to `0` only if you mount a bank volume yourself.

Optional tuning: `BANK_DIR`, `WINDOW`, `DRAW_MAX`, `STALE_AFTER_USES`, `SPAM_COUNT`,
`SPAM_WINDOW`, `SPAM_BAN`, `BURST_COUNT`, `BURST_BAN`, `NET_MAX_DRAWS`,
`GLOBAL_MAX_DRAWS`, `TRUSTED_PROXY_HOPS`.

Full documentation: [`relay/README.md`](https://github.com/archnemix/Archx-Eval/blob/main/relay/README.md).

## Privacy

No raw IP address, machine identifier or disk identifier is stored. They are
hashed with `RELAY_SALT` on arrival and discarded, so this Space, its disk and
the dataset hold 128-bit hashes and nothing that can be reversed into a person.
