"""Command line interface.

    archx-eval doctor                      check the endpoint and the relay
    archx-eval run --provider ollama ...   draw, answer, report
    archx-eval resume                      report a draw that was interrupted
    archx-eval status                      what is waiting to be reported

Exit codes are stable so this can sit in a script:
    0 success   1 usage or config error   2 the relay refused   3 the provider failed
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import __version__, device
from .errors import ConfigError, ProviderError, RelayError, RunnerError
from .providers import ProviderConfig
from .relay import DEFAULT_BASE_URL, RelayClient
from .session import (
    ItemResult,
    load_state,
    pending_sessions,
    report_session,
    run_session,
    save_state,
)
from .session import state_path

EXIT_OK = 0
EXIT_USAGE = 1
EXIT_RELAY = 2
EXIT_PROVIDER = 3

DEFAULT_STATE_DIR = Path.home() / ".local" / "state" / "archx-eval"


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    if not getattr(args, "command", None):
        parser.print_help()
        return EXIT_USAGE

    try:
        return args.func(args)
    except ConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except RelayError as exc:
        _explain_relay_refusal(exc)
        return EXIT_RELAY
    except ProviderError as exc:
        print(f"model error: {exc}", file=sys.stderr)
        return EXIT_PROVIDER
    except RunnerError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except KeyboardInterrupt:
        print("\ninterrupted. The draw is saved; run `archx-eval resume` to finish it.",
              file=sys.stderr)
        return 130


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="archx-eval",
        description="Draw questions from the Archx-Eval relay, ask a model, report the answers.",
    )
    parser.add_argument("--version", action="version", version=f"archx-eval {__version__}")
    parser.add_argument("--base-url", default=None, help=f"relay base URL (default {DEFAULT_BASE_URL})")
    parser.add_argument("--token", default=None, help="relay access token (or ARCHX_ACCESS_TOKEN)")
    parser.add_argument("--state-dir", default=None, help=f"where runs are saved (default {DEFAULT_STATE_DIR})")

    sub = parser.add_subparsers(dest="command")

    run = sub.add_parser("run", help="draw a set of questions and answer them")
    run.add_argument("--provider", default="ollama",
                     help="openai | anthropic | gemini | ollama | gguf | echo")
    run.add_argument("--model", required=False, default="",
                     help="model name, or a path when --provider gguf")
    run.add_argument("--provider-url", default=None,
                     help="override the provider base URL (a local server, a proxy)")
    run.add_argument("--api-key", default=None, help="provider key; prefer the usual env var")
    run.add_argument("--categories", default=None, help="comma-separated, e.g. python,logic")
    run.add_argument("--size", type=int, default=10, help="how many questions to request")
    run.add_argument("--max-tokens", type=int, default=1024)
    run.add_argument("--timeout", type=float, default=120.0, help="per-request timeout, seconds")
    run.add_argument("--report-only", action="store_true",
                     help="draw nothing; report a previously interrupted run")
    run.add_argument("--no-device", action="store_true",
                     help="do not send machine or disk ids (weaker identity, more likely to hit the window)")
    run.add_argument("--json", action="store_true", help="machine-readable output")
    run.set_defaults(func=cmd_run)

    resume = sub.add_parser("resume", help="report the most recent interrupted draw")
    resume.add_argument("--all", action="store_true", help="report every interrupted draw")
    resume.add_argument("--json", action="store_true")
    resume.set_defaults(func=cmd_resume)

    status = sub.add_parser("status", help="show draws that still need reporting")
    status.add_argument("--json", action="store_true")
    status.set_defaults(func=cmd_status)

    doctor = sub.add_parser("doctor", help="check connectivity, identity and stock")
    doctor.add_argument("--provider", default=None)
    doctor.add_argument("--model", default="")
    doctor.add_argument("--provider-url", default=None)
    doctor.add_argument("--json", action="store_true")
    doctor.set_defaults(func=cmd_doctor)

    return parser


# ---------------------------------------------------------------- commands ---


def cmd_run(args: argparse.Namespace) -> int:
    client, state_dir = _client(args)
    model_id = args.model or args.provider
    state_dir.mkdir(parents=True, exist_ok=True)

    if args.report_only:
        pending = pending_sessions(state_dir)
        if not pending:
            print("nothing to report: no interrupted runs found", file=sys.stderr)
            return EXIT_OK
        return _report(client, pending[0], state_dir, as_json=args.json)

    provider = ProviderConfig(
        provider=args.provider,
        model=args.model or _default_model(args.provider),
        base_url=args.provider_url,
        api_key=args.api_key,
        timeout=args.timeout,
    ).build()

    categories = [c.strip() for c in (args.categories or "").split(",") if c.strip()]
    grant = client.draw(
        categories=categories or None,
        size=args.size,
        send_device=not args.no_device,
    )

    if grant.shortfall:
        print(f"note: the relay served {len(grant.examples)} of {args.size} requested "
              f"({'; '.join(grant.notes)})", file=sys.stderr)

    print(f"drew {len(grant.examples)} question(s), request {grant.request_id}", file=sys.stderr)

    def progress(item: ItemResult, done: int, total: int) -> None:
        if args.json:
            return
        status = "error" if item.error else f"{item.latency_ms} ms"
        print(f"  [{done}/{total}] {item.example_id:<16} {status}", file=sys.stderr)

    state = run_session(
        client, provider, grant,
        state_dir=state_dir,
        model_id=model_id,
        max_tokens=args.max_tokens,
        on_item=progress,
    )

    response = report_session(client, state, state_dir=state_dir)
    if args.json:
        print(json.dumps({"request_id": grant.request_id, "model_id": model_id,
                          "response": response}, indent=2))
    else:
        summary = response.get("summary") or {}
        print(f"reported {summary.get('total', len(state.items))} answer(s) "
              f"for {model_id}; accepted={response.get('accepted')}")
        if grant.next_draw_at:
            print(f"next draw available at {grant.next_draw_at}")
    return EXIT_OK


def cmd_resume(args: argparse.Namespace) -> int:
    client, state_dir = _client(args)
    pending = pending_sessions(state_dir)
    if not pending:
        print("nothing to resume", file=sys.stderr)
        return EXIT_OK
    targets = pending if args.all else pending[:1]
    for path in targets:
        _report(client, path, state_dir, as_json=args.json)
    return EXIT_OK


def cmd_status(args: argparse.Namespace) -> int:
    _, state_dir = _client(args)
    rows = []
    for path in pending_sessions(state_dir):
        state = load_state(path)
        rows.append({
            "request_id": state.grant["request_id"],
            "model_id": state.model_id,
            "items": len(state.items),
            "expected": len(state.grant.get("examples", [])),
            "file": str(path),
        })
    if args.json:
        print(json.dumps(rows, indent=2))
    elif not rows:
        print("no unreported draws")
    else:
        for row in rows:
            print(f"{row['request_id']}  {row['model_id']}  "
                  f"{row['items']}/{row['expected']} answered")
    return EXIT_OK


def cmd_doctor(args: argparse.Namespace) -> int:
    client, _ = _client(args)
    report: dict[str, object] = {"runner_version": __version__, "base_url": client.base_url}

    ident = device.device_payload()
    report["device"] = {
        "machine_id_present": "machine_id" in ident,
        "disk_uuid_present": "disk_uuid" in ident,
        "os_family": ident.get("os_family"),
        "arch": ident.get("arch"),
    }

    try:
        health = client.health()
        report["relay"] = {
            "ok": health.get("ok"),
            "ready": (health.get("bank") or {}).get("_total", 0) > 0,
            "bank_total": (health.get("bank") or {}).get("_total", 0),
            "categories": sorted((health.get("bank") or {}).keys() - {"_total"}),
            "salt_is_default": health.get("salt_is_default"),
        }
    except RelayError as exc:
        report["relay"] = {"error": str(exc)}

    if args.provider:
        try:
            provider = ProviderConfig(
                provider=args.provider,
                model=args.model or _default_model(args.provider),
                base_url=args.provider_url,
            ).build()
            completion = provider.complete("Reply with the single word: ok", max_tokens=16)
            report["provider"] = {"ok": True, "sample": completion.text.strip()[:80]}
        except ProviderError as exc:
            report["provider"] = {"ok": False, "error": str(exc)}

    if args.json:
        print(json.dumps(report, indent=2))
    else:
        print(f"runner      {report['runner_version']}")
        print(f"relay       {client.base_url}")
        r = report.get("relay") or {}
        if "error" in r:
            print(f"            UNREACHABLE: {r['error']}")
        else:
            print(f"            ok={r['ok']} ready={r['ready']} bank={r['bank_total']} "
                  f"categories={', '.join(r['categories']) or 'none'}")
        d = report["device"]
        print(f"machine id  {'present' if d['machine_id_present'] else 'NOT AVAILABLE'}")
        print(f"disk id     {'present' if d['disk_uuid_present'] else 'NOT AVAILABLE'}")
        if "provider" in report:
            p = report["provider"]
            print(f"provider    {'ok' if p['ok'] else 'FAILED: ' + str(p.get('error'))}")
    return EXIT_OK


# ----------------------------------------------------------------- helpers ---


def _client(args: argparse.Namespace) -> tuple[RelayClient, Path]:
    token = args.token or _env("ARCHX_ACCESS_TOKEN")
    if not token:
        raise ConfigError(
            "no access token. Pass --token or set ARCHX_ACCESS_TOKEN.\n"
            "This is the ACCESS_TOKEN secret configured on the relay."
        )
    base = args.base_url or _env("ARCHX_BASE_URL") or DEFAULT_BASE_URL
    state_dir = Path(args.state_dir) if args.state_dir else DEFAULT_STATE_DIR
    return RelayClient(token, base), state_dir


def _env(name: str) -> str:
    import os

    return os.environ.get(name, "")


def _default_model(provider: str) -> str:
    return {
        "ollama": "llama3",
        "openai": "gpt-4o-mini",
        "anthropic": "claude-sonnet-4-20250514",
        "gemini": "gemini-2.0-flash",
        "echo": "echo",
        "gguf": "",
    }.get(provider, "")


def _report(client: RelayClient, path: Path, state_dir: Path, *, as_json: bool) -> int:
    state = load_state(path)
    try:
        response = report_session(client, state, state_dir=state_dir)
    except RelayError as exc:
        if exc.code == "ALREADY_REPORTED":
            print(f"{state.grant['request_id']}: already reported, marking done", file=sys.stderr)
            state.reported = True
            save_state(path, state)
            return EXIT_OK
        raise
    if as_json:
        print(json.dumps(response, indent=2))
    else:
        print(f"{state.grant['request_id']}: reported, accepted={response.get('accepted')}")
    return EXIT_OK


def _explain_relay_refusal(exc: RelayError) -> None:
    """Turn a machine code into something the user can act on."""
    print(f"the relay refused: {exc.code}", file=sys.stderr)
    if exc.message:
        print(f"  {exc.message}", file=sys.stderr)

    tips = {
        "WINDOW_ACTIVE": "This identity already drew in the current window. That is the normal "
                         "rule, not an error: wait for the time above.",
        "IDENTITY_BANNED": "This machine is serving a ban. Waiting is the only option; the "
                           "duration escalates if the pattern repeats.",
        "NETWORK_BANNED": "The network this machine is on is banned, usually because something "
                          "else on it flooded the relay. Try a different connection.",
        "NETWORK_CEILING": "This network has hit its draw ceiling for the window. "
                           "Wait or use a different connection.",
        "GLOBAL_CEILING": "The relay is at capacity. This is temporary.",
        "SPAM_RATE": "Requests arrived faster than the relay allows. The runner does not retry "
                     "draws on its own, so this usually means another tool is using the same "
                     "connection.",
        "PRIVACY_BLOCKED": "Requests from VPN, Tor or hosting ranges are refused by design. "
                           "Run the runner from a normal connection.",
        "ACCESS_DENIED": "The access token is wrong. Check ARCHX_ACCESS_TOKEN.",
        "UNKNOWN_REQUEST": "This draw is not known to the relay. If it drew more than the "
                           "feedback window ago, its results can no longer be accepted.",
        "ALREADY_REPORTED": "These results were already accepted once.",
    }
    if exc.code in tips:
        print(f"  -> {tips[exc.code]}", file=sys.stderr)


if __name__ == "__main__":
    raise SystemExit(main())
