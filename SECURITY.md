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
| Reading raw identity data | hashes only in the mirrored tree; raw values live in the private dataset and expire |
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
  every remembered identity a stranger, which resets everyone's window.
- `ALLOW_LOOPBACK` must be unset in production.
- Persistent storage must be mounted at `/app/data` and `/app/rate`, or a
  restart resets identity memory and rate history.
- The Space must remain private; it is not a public API.
- Rotate `ACCESS_TOKEN` and `ADMIN_TOKEN` if a runner image leaks.
