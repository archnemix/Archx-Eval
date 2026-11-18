"""HTTP client for the relay.

Every refusal the relay can return is raised as a RelayError carrying the code
and, where the relay supplied one, the Retry-After wait. This module never
retries a draw on its own: a draw is a scarce resource (one per identity per
window), so deciding to try again is the caller's business, not a hidden loop's.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Any

from . import __version__, device
from .errors import RelayError, RunnerError

DEFAULT_BASE_URL = "https://api-relay-archx-eval.hf.space"
DEFAULT_TIMEOUT = 60.0


@dataclass
class Example:
    """One question, as the relay renders it.

    Note what is absent: reference_solution, tests, expected_answer and rubric
    are stripped server-side. The runner genuinely cannot see them.
    """

    id: str
    category: str
    difficulty: str
    kind: str
    prompt: str
    context: str | None = None
    language: str | None = None
    entrypoint: str | None = None
    starter_code: str | None = None
    tags: list[str] = field(default_factory=list)

    @classmethod
    def from_json(cls, raw: dict[str, Any]) -> Example:
        return cls(
            id=raw["id"],
            category=raw.get("category", ""),
            difficulty=raw.get("difficulty", ""),
            kind=raw.get("kind", ""),
            prompt=raw["prompt"],
            context=raw.get("context"),
            language=raw.get("language"),
            entrypoint=raw.get("entrypoint"),
            starter_code=raw.get("starter_code"),
            tags=raw.get("tags") or [],
        )


@dataclass
class Grant:
    """A successful draw. request_id is the handle every report must quote."""

    request_id: str
    identity_id: str
    examples: list[Example]
    issued_at: str | None = None
    next_draw_at: str | None = None
    shortfall: int = 0
    notes: list[str] = field(default_factory=list)

    @classmethod
    def from_json(cls, raw: dict[str, Any]) -> Grant:
        return cls(
            request_id=raw["request_id"],
            identity_id=raw["identity_id"],
            examples=[Example.from_json(e) for e in raw.get("examples", [])],
            issued_at=raw.get("issued_at"),
            next_draw_at=raw.get("next_draw_at"),
            shortfall=raw.get("shortfall") or 0,
            notes=raw.get("notes") or [],
        )


class RelayClient:
    """Talks to the relay. One instance per process is enough."""

    def __init__(
        self,
        access_token: str,
        base_url: str = DEFAULT_BASE_URL,
        *,
        timeout: float = DEFAULT_TIMEOUT,
        user_agent: str | None = None,
    ) -> None:
        if not access_token:
            raise ValueError("an access token is required: the relay refuses draws without one")
        self.base_url = base_url.rstrip("/")
        self.access_token = access_token
        self.timeout = timeout
        self.user_agent = user_agent or f"archx-eval/{__version__}"

    # ------------------------------------------------------------------ draw --

    def draw(
        self,
        *,
        categories: list[str] | None = None,
        size: int = 10,
        mix: dict[str, float] | None = None,
        send_device: bool = True,
    ) -> Grant:
        """Ask for a set of questions.

        Raises RelayError on any refusal. In particular WINDOW_ACTIVE means this
        identity has already drawn inside the current window: that is the normal
        rule, not a fault, and the caller should surface the wait rather than
        retry immediately.
        """
        body: dict[str, Any] = {"size": int(size)}
        wanted = list(categories or [])
        if not wanted and not mix:
            # The relay refuses a draw with no categories, and the caller
            # almost always means "whatever is in the bank". Asking the bank
            # is one extra request and removes a pointless failure mode.
            wanted = self.categories()
            if not wanted:
                raise RunnerError(
                    "the relay reported no categories, so there is nothing to draw"
                )
        if wanted:
            body["categories"] = wanted
        if mix:
            body["mix"] = dict(mix)
        if send_device:
            body["device"] = device.device_payload()

        raw = self._post("/v1/draw", body, authenticated=True)
        return Grant.from_json(raw)

    # -------------------------------------------------------------- feedback --

    def report(
        self,
        grant: Grant,
        items: list[dict[str, Any]],
        *,
        model_id: str,
        partial: bool = False,
        duration_s: float | None = None,
    ) -> dict[str, Any]:
        """Send the answers for a grant.

        items must cover every example that was issued, unless partial is set.
        The relay rejects an incomplete report without that flag, because
        dropping the questions a model did badly on would silently raise its
        score.
        """
        body: dict[str, Any] = {
            "request_id": grant.request_id,
            # The relay compares this against the identity the draw was issued
            # to, so a report cannot be filed under someone else.
            "identity_id": grant.identity_id,
            "model_id": model_id,
            "runner_version": __version__,
            "partial": partial,
            "items": items,
        }
        if duration_s is not None:
            body["duration_s"] = float(duration_s)
        return self._post("/v1/feedback", body, authenticated=False)

    # ------------------------------------------------------------ discovery ---

    def health(self) -> dict[str, Any]:
        return self._get("/health")

    def categories(self) -> list[str]:
        raw = self._get("/v1/categories")
        return list(raw.get("categories") or [])

    # -------------------------------------------------------------- plumbing --

    def _get(self, path: str) -> dict[str, Any]:
        return self._request("GET", path, None, authenticated=False)

    def _post(self, path: str, body: dict[str, Any], *, authenticated: bool) -> dict[str, Any]:
        return self._request("POST", path, body, authenticated=authenticated)

    def _request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None,
        *,
        authenticated: bool,
    ) -> dict[str, Any]:
        url = f"{self.base_url}{path}"
        data = json.dumps(body).encode() if body is not None else None

        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", self.user_agent)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if authenticated:
            req.add_header("Authorization", f"Bearer {self.access_token}")

        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                payload = resp.read()
        except urllib.error.HTTPError as exc:
            raise self._error_from(exc) from None
        except urllib.error.URLError as exc:
            raise RelayError(
                status=0,
                code="UNREACHABLE",
                message=f"could not reach the relay at {self.base_url}: {exc.reason}",
            ) from None
        except TimeoutError:
            raise RelayError(
                status=0,
                code="TIMEOUT",
                message=f"the relay did not answer within {self.timeout:.0f}s",
            ) from None

        if not payload:
            return {}
        try:
            return json.loads(payload)
        except json.JSONDecodeError:
            raise RelayError(
                status=0,
                code="BAD_RESPONSE",
                message="the relay returned something that is not JSON",
            ) from None

    @staticmethod
    def _error_from(exc: urllib.error.HTTPError) -> RelayError:
        retry_after = _parse_retry_after(exc.headers.get("Retry-After")) if exc.headers else None
        code, message = "HTTP_ERROR", ""
        try:
            raw = json.loads(exc.read() or b"{}")
            err = raw.get("error") or {}
            code = err.get("code") or code
            message = err.get("message") or ""
        except (json.JSONDecodeError, OSError):
            pass
        return RelayError(
            status=exc.code,
            code=code,
            message=message or (exc.reason or "no detail supplied"),
            retry_after=retry_after,
        )


def _parse_retry_after(value: str | None) -> float | None:
    if not value:
        return None
    try:
        return float(value)
    except ValueError:
        pass
    # The header may also be an HTTP date. Anything unparseable becomes a small
    # default rather than a crash, since it is only used for display.
    try:
        from email.utils import parsedate_to_datetime

        when = parsedate_to_datetime(value)
        return max(0.0, when.timestamp() - time.time())
    except Exception:  # noqa: BLE001
        return None
