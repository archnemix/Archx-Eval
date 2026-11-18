"""Running a draw: prompt building, resumable state, and reporting.

The important design constraint is that a draw is expensive. One identity may
draw once per window, so if the runner is killed after drawing and before
reporting, that window is spent and the results are lost. State is therefore
written to disk the moment a grant arrives, and a later run can resume it.

That resumption is also why the state file is written with the answers as they
are produced: a long run against a slow local model can take hours, and losing
it to a closed laptop lid would be a real cost to the user.
"""

from __future__ import annotations

import json
import os
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from . import __version__
from .errors import ProviderError, RunnerError
from .providers import Provider
from .relay import Example, Grant, RelayClient

STATE_VERSION = 1


def build_prompt(example: Example, *, system_preamble: str | None = None) -> str:
    """Turn a question into a single prompt.

    Deliberately plain text rather than a chat template: the runner does not know
    which template the model was trained on, and guessing wrong silently costs
    accuracy. Providers that need a chat format wrap this themselves.
    """
    parts: list[str] = []
    if system_preamble:
        parts.append(system_preamble)

    if example.kind == "code":
        header = f"Language: {example.language or 'python'}"
        if example.entrypoint:
            header += f"\nImplement a function named `{example.entrypoint}`."
        parts.append(header)
        parts.append(example.prompt)
        if example.context:
            parts.append(example.context)
        if example.starter_code:
            parts.append("Starting point:\n```\n" + example.starter_code.rstrip() + "\n```")
        parts.append(
            "Reply with the complete solution inside a single fenced code block. "
            "No explanation."
        )
    else:
        if example.context:
            parts.append(example.context)
        parts.append(example.prompt)
        parts.append("Answer directly and concisely.")

    return "\n\n".join(p for p in parts if p)


def extract_answer(example: Example, raw: str) -> tuple[str, str]:
    """Split a model reply into (answer, code).

    Code questions want the fenced block if there is one; falling back to the
    whole reply keeps a model that ignored the instruction scoreable rather than
    discarded.
    """
    if example.kind != "code":
        return raw.strip(), ""

    fence = "```"
    start = raw.find(fence)
    if start == -1:
        return "", raw.strip()
    after = raw.find("\n", start)
    if after == -1:
        return "", raw.strip()
    end = raw.find(fence, after)
    if end == -1:
        return "", raw[after:].strip()
    code = raw[after + 1 : end].strip()
    return "", code or raw.strip()


@dataclass
class ItemResult:
    example_id: str
    category: str
    difficulty: str
    answer: str = ""
    code: str = ""
    passed: bool = False
    score: float = 0.0
    latency_ms: int = 0
    error: str | None = None

    def to_payload(self) -> dict[str, Any]:
        return {
            "example_id": self.example_id,
            "category": self.category,
            "difficulty": self.difficulty,
            "answer": self.answer,
            "code": self.code,
            "passed": self.passed,
            "score": self.score,
            "latency_ms": self.latency_ms,
            **({"error": self.error} if self.error else {}),
        }


@dataclass
class SessionState:
    """Everything needed to resume or report a draw."""

    version: int
    grant: dict[str, Any]
    model_id: str
    provider: str
    started_at: float
    finished_at: float | None = None
    reported: bool = False
    items: list[dict[str, Any]] = field(default_factory=list)

    @property
    def done_ids(self) -> set[str]:
        return {i["example_id"] for i in self.items}


def state_path(state_dir: Path, request_id: str) -> Path:
    return state_dir / f"{request_id}.json"


def save_state(path: Path, state: SessionState) -> None:
    """Write state atomically, so a crash mid-write cannot corrupt a spent draw."""
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(asdict(state), indent=2), encoding="utf-8")
    os.replace(tmp, path)


def load_state(path: Path) -> SessionState:
    raw = json.loads(path.read_text(encoding="utf-8"))
    if raw.get("version") != STATE_VERSION:
        raise RunnerError(
            f"{path} was written by a different runner version "
            f"({raw.get('version')}, expected {STATE_VERSION}); it cannot be resumed"
        )
    return SessionState(**raw)


def pending_sessions(state_dir: Path) -> list[Path]:
    """Unreported sessions, newest first. These have already cost a draw."""
    if not state_dir.is_dir():
        return []
    out: list[Path] = []
    for path in state_dir.glob("*.json"):
        try:
            state = load_state(path)
        except (RunnerError, json.JSONDecodeError, TypeError, KeyError):
            continue
        if not state.reported:
            out.append(path)
    out.sort(key=lambda p: p.stat().st_mtime, reverse=True)
    return out


def run_session(
    client: RelayClient,
    provider: Provider,
    grant: Grant,
    *,
    state_dir: Path,
    model_id: str,
    system_preamble: str | None = None,
    max_tokens: int = 1024,
    on_item: Any = None,
    resume: bool = True,
) -> SessionState:
    """Answer every question in a grant, persisting as it goes.

    A provider failure on one question is recorded as an error on that item and
    the run continues: one flaky response should not discard a whole draw. The
    report is then sent marked partial, so the server knows some questions are
    unanswered rather than silently scoring them as wrong.
    """
    path = state_path(state_dir, grant.request_id)
    if resume and path.exists():
        state = load_state(path)
    else:
        state = SessionState(
            version=STATE_VERSION,
            grant=_grant_to_json(grant),
            model_id=model_id,
            provider=getattr(provider, "name", "unknown"),
            started_at=time.time(),
        )
        save_state(path, state)

    done = state.done_ids
    for example in grant.examples:
        # Checked per item, not once up front: a grant is a list and nothing
        # stops it repeating an id. Answering twice would make the whole report
        # unusable, because the relay rejects a duplicate as a bad request.
        if example.id in done:
            continue

        started = time.time()
        try:
            completion = provider.complete(build_prompt(example, system_preamble=system_preamble),
                                           max_tokens=max_tokens)
            answer, code = extract_answer(example, completion.text)
            item = ItemResult(
                example_id=example.id,
                category=example.category,
                difficulty=example.difficulty,
                answer=answer,
                code=code,
                latency_ms=int((time.time() - started) * 1000),
            )
        except ProviderError as exc:
            item = ItemResult(
                example_id=example.id,
                category=example.category,
                difficulty=example.difficulty,
                latency_ms=int((time.time() - started) * 1000),
                error=str(exc)[:500],
            )

        state.items.append(asdict(item))
        done.add(example.id)
        save_state(path, state)
        if on_item is not None:
            on_item(item, len(state.items), len(grant.examples))

    return state


def report_session(
    client: RelayClient,
    state: SessionState,
    *,
    state_dir: Path,
) -> dict[str, Any]:
    """Send a finished session and mark it reported.

    partial is set whenever any item carries an error or is missing, so the
    server records an honest, incomplete run instead of treating unanswered
    questions as failures.
    """
    grant = Grant.from_json(state.grant)

    # One answer per issued question, first wins. A state file written while the
    # relay could still hand out the same example twice would otherwise contain
    # duplicates, and the relay rejects the entire report for that -- losing a
    # draw that was already paid for. Dropping the extra answer and marking the
    # report partial keeps the run usable.
    seen: set[str] = set()
    items: list[dict[str, Any]] = []
    for item in state.items:
        eid = item.get("example_id", "")
        if eid in seen:
            continue
        seen.add(eid)
        items.append(item)
    deduped = len(items) != len(state.items)

    issued = [e.id for e in grant.examples]
    have = {i["example_id"] for i in items}
    missing = [eid for eid in issued if eid not in have]
    errored = [i for i in items if i.get("error")]
    partial = deduped or bool(missing) or bool(errored)

    duration = max(0.0, (state.finished_at or time.time()) - state.started_at)
    response = client.report(
        grant,
        items,
        model_id=state.model_id,
        partial=partial,
        duration_s=duration,
    )

    state.reported = True
    state.finished_at = state.finished_at or time.time()
    save_state(state_path(state_dir, grant.request_id), state)
    return response


def _grant_to_json(grant: Grant) -> dict[str, Any]:
    return {
        "request_id": grant.request_id,
        "identity_id": grant.identity_id,
        "issued_at": grant.issued_at,
        "next_draw_at": grant.next_draw_at,
        "shortfall": grant.shortfall,
        "notes": grant.notes,
        "examples": [
            {
                "id": e.id,
                "category": e.category,
                "difficulty": e.difficulty,
                "kind": e.kind,
                "prompt": e.prompt,
                "context": e.context,
                "language": e.language,
                "entrypoint": e.entrypoint,
                "starter_code": e.starter_code,
                "tags": e.tags,
            }
            for e in grant.examples
        ],
    }


def runner_version() -> str:
    return __version__
