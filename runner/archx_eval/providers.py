"""Model backends.

One interface, several transports. Everything here answers a single question:
given a prompt, what did the model say? Grading is not in this file and never
will be, because the answer key is not on this machine.

Supported, selected with --provider:

    openai       OpenAI, and anything speaking the same /chat/completions API
                 (vLLM, LM Studio, llama.cpp server, Together, Groq, OpenRouter)
    anthropic    Claude, via /v1/messages
    gemini       Google, via generateContent
    ollama       a local Ollama daemon
    gguf         a local .gguf file, run in-process with llama-cpp-python
    echo         no model at all; echoes the prompt. For testing the runner.

There is deliberately no "agent" mode. If the evaluated system can invoke the
runner, it can obtain the draw and the questions directly, and the evaluation
means nothing.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Protocol

from . import __version__
from .errors import ProviderError


@dataclass
class Completion:
    """One model answer, plus the facts worth recording about it."""

    text: str
    latency_ms: int = 0
    error: str | None = None
    # Tokens are reported when the backend volunteers them; never required.
    prompt_tokens: int | None = None
    completion_tokens: int | None = None


class Provider(Protocol):
    name: str

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion: ...


# --------------------------------------------------------------------- HTTP ---


def _post_json(url: str, payload: dict[str, Any], headers: dict[str, str], timeout: float) -> dict[str, Any]:
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json")
    req.add_header("User-Agent", f"archx-eval/{__version__}")
    for key, value in headers.items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as exc:
        detail = ""
        try:
            detail = exc.read().decode(errors="replace")[:400]
        except OSError:
            pass
        # 429 and 5xx are worth another attempt; 4xx generally is not.
        raise ProviderError(
            f"{exc.code} from {url}: {detail or exc.reason}",
            retryable=exc.code == 429 or exc.code >= 500,
        ) from None
    except urllib.error.URLError as exc:
        raise ProviderError(f"cannot reach {url}: {exc.reason}", retryable=True) from None
    except (TimeoutError, json.JSONDecodeError) as exc:
        raise ProviderError(f"bad response from {url}: {exc}", retryable=True) from None


class OpenAICompatible:
    """Any /chat/completions endpoint.

    This is the widest net: OpenAI itself, plus vLLM, LM Studio, llama.cpp's
    server, Together, Groq and OpenRouter all speak it. Point --base-url at the
    local server and no API key is needed.
    """

    name = "openai"

    def __init__(
        self,
        model: str,
        *,
        base_url: str = "https://api.openai.com/v1",
        api_key: str | None = None,
        timeout: float = 120.0,
        temperature: float = 0.0,
    ) -> None:
        self.model = model
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key or os.environ.get("OPENAI_API_KEY") or ""
        self.timeout = timeout
        self.temperature = temperature

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
        headers = {}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        payload = {
            "model": self.model,
            "messages": [{"role": "user", "content": prompt}],
            "temperature": self.temperature,
            "max_tokens": max_tokens,
        }
        raw = _post_json(f"{self.base_url}/chat/completions", payload, headers, self.timeout)
        try:
            text = raw["choices"][0]["message"]["content"] or ""
        except (KeyError, IndexError, TypeError):
            raise ProviderError(f"unexpected response shape: {str(raw)[:300]}") from None
        usage = raw.get("usage") or {}
        return Completion(
            text=text,
            prompt_tokens=usage.get("prompt_tokens"),
            completion_tokens=usage.get("completion_tokens"),
        )


class Anthropic:
    name = "anthropic"

    def __init__(
        self,
        model: str,
        *,
        base_url: str = "https://api.anthropic.com",
        api_key: str | None = None,
        timeout: float = 120.0,
    ) -> None:
        self.model = model
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key or os.environ.get("ANTHROPIC_API_KEY") or ""
        self.timeout = timeout

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
        if not self.api_key:
            raise ProviderError("ANTHROPIC_API_KEY is not set")
        payload = {
            "model": self.model,
            "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}],
        }
        raw = _post_json(
            f"{self.base_url}/v1/messages",
            payload,
            {"x-api-key": self.api_key, "anthropic-version": "2023-06-01"},
            self.timeout,
        )
        blocks = raw.get("content") or []
        text = "".join(b.get("text", "") for b in blocks if isinstance(b, dict))
        usage = raw.get("usage") or {}
        return Completion(
            text=text,
            prompt_tokens=usage.get("input_tokens"),
            completion_tokens=usage.get("output_tokens"),
        )


class Gemini:
    name = "gemini"

    def __init__(
        self,
        model: str,
        *,
        base_url: str = "https://generativelanguage.googleapis.com/v1beta",
        api_key: str | None = None,
        timeout: float = 120.0,
    ) -> None:
        self.model = model
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key or os.environ.get("GEMINI_API_KEY") or os.environ.get("GOOGLE_API_KEY") or ""
        self.timeout = timeout

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
        if not self.api_key:
            raise ProviderError("GEMINI_API_KEY is not set")
        payload = {
            "contents": [{"parts": [{"text": prompt}]}],
            "generationConfig": {"maxOutputTokens": max_tokens, "temperature": 0.0},
        }
        raw = _post_json(
            f"{self.base_url}/models/{self.model}:generateContent?key={self.api_key}",
            payload,
            {},
            self.timeout,
        )
        try:
            parts = raw["candidates"][0]["content"]["parts"]
            text = "".join(p.get("text", "") for p in parts if isinstance(p, dict))
        except (KeyError, IndexError, TypeError):
            raise ProviderError(f"unexpected response shape: {str(raw)[:300]}") from None
        usage = raw.get("usageMetadata") or {}
        return Completion(
            text=text,
            prompt_tokens=usage.get("promptTokenCount"),
            completion_tokens=usage.get("candidatesTokenCount"),
        )


class Ollama:
    """A local Ollama daemon. No key, no network beyond localhost."""

    name = "ollama"

    def __init__(
        self,
        model: str,
        *,
        base_url: str = "http://localhost:11434",
        timeout: float = 300.0,
    ) -> None:
        self.model = model
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
        payload = {
            "model": self.model,
            "prompt": prompt,
            "stream": False,
            "options": {"temperature": 0.0, "num_predict": max_tokens},
        }
        raw = _post_json(f"{self.base_url}/api/generate", payload, {}, self.timeout)
        text = raw.get("response")
        if text is None:
            raise ProviderError(f"unexpected response shape: {str(raw)[:300]}")
        return Completion(
            text=text,
            prompt_tokens=raw.get("prompt_eval_count"),
            completion_tokens=raw.get("eval_count"),
        )


class GGUFLocal:
    """A .gguf file run in-process.

    llama-cpp-python is an optional dependency: importing it at module level
    would make the whole package heavy for people who only use an HTTP endpoint.
    """

    name = "gguf"

    def __init__(
        self,
        model: str,
        *,
        n_ctx: int = 8192,
        n_gpu_layers: int = -1,
        timeout: float = 600.0,
    ) -> None:
        self.model_path = model
        self.n_ctx = n_ctx
        self.n_gpu_layers = n_gpu_layers
        self.timeout = timeout
        self._llm: Any = None

    def _load(self) -> Any:
        if self._llm is not None:
            return self._llm
        try:
            from llama_cpp import Llama
        except ImportError:
            raise ProviderError(
                "reading a .gguf directly needs llama-cpp-python:\n"
                "    pip install 'archx-eval[gguf]'\n"
                "or point --base-url at a llama.cpp server or Ollama instead."
            ) from None
        try:
            self._llm = Llama(
                model_path=self.model_path,
                n_ctx=self.n_ctx,
                n_gpu_layers=self.n_gpu_layers,
                verbose=False,
            )
        except Exception as exc:  # noqa: BLE001 - llama.cpp raises broadly
            raise ProviderError(f"could not load {self.model_path}: {exc}") from None
        return self._llm

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
        llm = self._load()
        try:
            out = llm(prompt, max_tokens=max_tokens, temperature=0.0)
        except Exception as exc:  # noqa: BLE001
            raise ProviderError(f"generation failed: {exc}") from None
        text = ""
        try:
            text = out["choices"][0]["text"]
        except (KeyError, IndexError, TypeError):
            pass
        usage = out.get("usage") or {}
        return Completion(
            text=text,
            prompt_tokens=usage.get("prompt_tokens"),
            completion_tokens=usage.get("completion_tokens"),
        )


class Echo:
    """No model. Returns the prompt itself.

    Exists so the runner's own plumbing can be tested end to end without a GPU
    or an API key. It is honest about what it is: the feedback it produces comes
    from the server, and its scores will be whatever echoing the prompt earns.
    """

    name = "echo"

    def __init__(self, model: str = "echo", **_: Any) -> None:
        self.model = model

    def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
        return Completion(text=prompt[:max_tokens])


PROVIDERS = {
    "openai": OpenAICompatible,
    "anthropic": Anthropic,
    "gemini": Gemini,
    "ollama": Ollama,
    "gguf": GGUFLocal,
    "echo": Echo,
}


@dataclass
class ProviderConfig:
    """Everything needed to build a provider, from flags or a config file."""

    provider: str
    model: str
    base_url: str | None = None
    api_key: str | None = None
    timeout: float = 120.0
    extra: dict[str, Any] = field(default_factory=dict)

    def build(self) -> Provider:
        cls = PROVIDERS.get(self.provider)
        if cls is None:
            known = ", ".join(sorted(PROVIDERS))
            raise ProviderError(f"unknown provider {self.provider!r}; choose one of: {known}")
        kwargs: dict[str, Any] = {"timeout": self.timeout}
        if self.base_url:
            kwargs["base_url"] = self.base_url
        if self.api_key:
            kwargs["api_key"] = self.api_key
        kwargs.update(self.extra)
        return cls(self.model, **kwargs)
