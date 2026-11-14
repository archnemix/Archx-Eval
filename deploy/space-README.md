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

This Space is deployed automatically from
[`archnemix/Archx-Eval`](https://github.com/archnemix/Archx-Eval) by the
`hf-space` workflow. Do not edit files here directly: the next deploy overwrites
them.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/health` | none | readiness and bank inventory |
| `GET` | `/v1/categories` | none | servable categories |
| `GET` | `/metrics` | metrics token | Prometheus exposition |
| `POST` | `/v1/draw` | access token | request up to 75 examples |
| `POST` | `/v1/feedback` | none | report runner results |
| `GET` | `/v1/stale` | admin token | examples at the use limit |
| `POST` | `/v1/erase` | admin token | delete the raw row for an identity |

## Required secrets

Set these in **Settings → Variables and secrets** for this Space:

| Name | Purpose |
|---|---|
| `RELAY_SALT` | keys every marker hash. Set once, never change it. |
| `ACCESS_TOKEN` | required on every draw |
| `ADMIN_TOKEN` | gates the operational endpoints |
| `HF_TOKEN` | **must be a token belonging to the Space's own account** with read+write on the private dataset |
| `METRICS_TOKEN` | optional; gates `/metrics` |

Optional tuning: `WINDOW`, `DRAW_MAX`, `STALE_AFTER_USES`, `SPAM_COUNT`,
`SPAM_WINDOW`, `SPAM_BAN`, `BURST_COUNT`, `BURST_BAN`, `NET_MAX_DRAWS`,
`GLOBAL_MAX_DRAWS`, `TRUSTED_PROXY_HOPS`.

Full documentation: [`relay/README.md`](https://github.com/archnemix/Archx-Eval/blob/main/relay/README.md).
