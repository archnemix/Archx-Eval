"""Errors, with the relay's refusals mapped onto something a human can act on.

The relay is deliberately terse with machine codes. This module is where those
codes become instructions: "you already drew in this window, come back in 47
minutes" rather than a bare 429.
"""

from __future__ import annotations

from dataclasses import dataclass


class RunnerError(Exception):
    """Base class for everything this package raises on purpose."""


class ConfigError(RunnerError):
    """The invocation itself is wrong; no request was attempted."""


@dataclass
class RelayError(RunnerError):
    """The relay refused a request.

    status and code come straight from the response. retry_after is in seconds
    when the relay supplied it, which it does for every rate refusal.
    """

    status: int
    code: str
    message: str
    retry_after: float | None = None

    def __str__(self) -> str:
        base = f"{self.status} {self.code}: {self.message}"
        if self.retry_after:
            base += f" (retry in {humanise(self.retry_after)})"
        return base

    @property
    def is_rate_limited(self) -> bool:
        """A refusal the caller can wait out, as opposed to a permanent no."""
        return self.code in {
            "WINDOW_ACTIVE",
            "SPAM_RATE",
            "IDENTITY_BANNED",
            "NETWORK_BANNED",
            "NETWORK_CEILING",
            "GLOBAL_CEILING",
        }

    @property
    def is_identity_problem(self) -> bool:
        """Refusals that come from how the machine looks, not from the user.

        These are worth surfacing clearly: they usually mean the runner is on a
        VPN, Tor, a cloud host, or a shared network that has already been
        banned, and nothing the user does to the runner will change it.
        """
        return self.code in {"PRIVACY_BLOCKED", "TOR_EXIT", "IDENTITY_INVALID"}


class ProviderError(RunnerError):
    """The model endpoint failed, so no answer could be produced."""

    def __init__(self, message: str, *, retryable: bool = False) -> None:
        super().__init__(message)
        self.retryable = retryable


def humanise(seconds: float) -> str:
    """Render a wait the way a person reads it."""
    seconds = max(0, int(round(seconds)))
    if seconds < 60:
        return f"{seconds} second{'s' if seconds != 1 else ''}"
    minutes = seconds // 60
    if minutes < 60:
        return f"{minutes} minute{'s' if minutes != 1 else ''}"
    hours = minutes / 60
    if hours < 48:
        rounded = round(hours)
        return f"{rounded} hour{'s' if rounded != 1 else ''}"
    days = round(hours / 24)
    return f"{days} day{'s' if days != 1 else ''}"
