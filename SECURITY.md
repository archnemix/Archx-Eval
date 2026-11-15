# Security policy

## Reporting

Open a private security advisory on this repository. Do not file a public issue
for anything that could be used to bypass the draw limits.

## Threat model

The system's job is to make mass or bulk extraction of the evaluation bank
expensive, visible, and attributable. It is **not** designed to defeat a
determined attacker with many machines and many identities: nothing short of
per-user accounts, payment and manual review solves that. What is implemented
is defence that raises the cost of the lazy and automated paths by orders of
magnitude, while making the determined path detectable.

### In scope

| Threat | Control |
|---|---|
| Bulk scraping with one device | 1-hour window keyed on a marker set; rotating one attribute does not reset it |
| Rotating IP only | markers match on IP **or** machine id **or** disk id |
| Restarting the service to clear limits | durable rate log; limits survive process restarts |
| Flooding to exhaust the bank | burst and sustained ladders, plus subnet and global ceilings |
| Using our infrastructure as free storage | body caps, per-field caps, 500 items per report |
| Reading answers from the API | responses never serialise solutions, tests, answers or explanations |
| Reading raw identity data | **nothing raw is ever stored** — no IP, no machine id, no disk id on disk, in git, or in logs |
| Credential stuffing against the API | constant-time token comparison; no token means the endpoint is closed, not open |
| Leaking existence of endpoints | uniform error shapes; metrics return the same error for wrong token and disabled state |
| Log injection via submitted text | control characters rejected in identifiers |
| Cache or proxy serving a draw twice | `no-store` on every response |

### Out of scope, by design

- A user with a genuinely fresh machine and a genuinely fresh network each hour.
  That is indistinguishable from a new legitimate user, and treating it as abuse
  would break honest users.
- Client-side honesty. `machine_id` and `disk_uuid` are self-reported. The
  controls that actually matter are the server-side ones.
- Availability under a distributed denial of service. That belongs to the
  platform's edge.

## What is not stored, and why that matters more

No raw IP address and no raw machine or disk identifier is written anywhere: not
to the data directory, not to the dataset, not to the logs. The values exist only
as arguments inside `identity.FromRequest`, are salted-HMAC'd immediately, and
are discarded with the stack frame. The on-disk record for a returning caller is:

```json
{
  "id": "b3181136b958bbfa08e0e8a5dd93ad57",
  "markers": ["31fff368...", "aa1e58e2...", "3df609f7..."],
  "requests": 2,
  "draws": 1
}
```

A salted HMAC with a 32-byte key is a one-way function: a full leak of the data
directory, the dataset, or the Space's storage yields nothing that can be turned
back into a person, and no record that can be subpoenaed as personal data.
Retention therefore has no obligation to satisfy — the 30-day purge requirement
disappeared with the raw values. `TestNoRawIdentityAnywhereOnDisk` sweeps every
byte the service writes after a full request cycle and fails if any raw value
appears; `TestAccessLogContainsNoRawAddress` covers the HTTP log.

The trade-off, stated plainly: if the salt leaks **and** an attacker obtains a
candidate list of IP addresses, they can test which candidates appear. Mitigate
by treating `RELAY_SALT` as the single most sensitive value in the system.

## Deliberate limitations we accept and document

1. **Identity values are attacker-supplied.** Anything a client sends can be
   forged. We rely on the cost of forging *all* markers, not on the truthfulness
   of any one.
2. **The dataset is the secret.** The bank itself lives in a private dataset.
   The relay never ships a copy of the bank to a Space image.
3. **No true client authentication.** A shared access token is a barrier, not an
   identity. Per-runner keys are the next step.

## Operational requirements

- `RELAY_SALT` must be set, long, random, and never rotated. Rotating it makes
  every remembered identity a stranger, which resets everyone's window. It is
  also the only thing standing between a leaked dataset and a dictionary attack
  on the hashes, so it deserves the strongest handling of any value here.
- `ALLOW_LOOPBACK` must be unset in production.
- Persistent storage must be mounted at `/app/data` and `/app/rate`, or a
  restart resets rate history. (Identities and events can be restored from the
  dataset; the rate log cannot.)
- The Space must remain private; it is not a public API.
- Rotate `ACCESS_TOKEN` and `ADMIN_TOKEN` if a runner image leaks.
