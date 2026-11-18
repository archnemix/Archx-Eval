"""Tests for the front runner.

Two kinds here, and the second kind is the one that matters:

  * unit tests over prompt building, answer extraction and state handling;
  * an end-to-end test that starts the real relay binary, draws from it, answers
    with a fake provider, and reports — proving the contract the runner was
    built against actually holds.

The end-to-end test skips cleanly when the relay binary is not available, so the
package can be tested on a machine that only has Python.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from archx_eval import device  # noqa: E402
from archx_eval.errors import RelayError  # noqa: E402
from archx_eval.providers import Completion, Echo  # noqa: E402
from archx_eval.relay import Example, Grant, RelayClient  # noqa: E402
from archx_eval.session import (  # noqa: E402
    SessionState,
    build_prompt,
    extract_answer,
    load_state,
    pending_sessions,
    report_session,
    run_session,
    save_state,
    state_path,
)

REPO_ROOT = Path(__file__).resolve().parents[2]
RELAY_DIR = REPO_ROOT / "relay"


# ------------------------------------------------------------------- unit ---


def make_example(kind: str = "code", **kw) -> Example:
    base = dict(
        id="py-easy-001",
        category="python",
        difficulty="easy",
        kind=kind,
        prompt="Return the sum of two integers a and b.",
        language="python",
        entrypoint="solve",
        starter_code="def solve(a, b):\n    pass\n",
    )
    base.update(kw)
    return Example(**base)


def test_code_prompt_asks_for_a_fenced_block():
    prompt = build_prompt(make_example())
    assert "Implement a function named `solve`" in prompt
    assert "Return the sum of two integers" in prompt
    assert "fenced code block" in prompt


def test_qa_prompt_does_not_mention_code():
    prompt = build_prompt(make_example(kind="qa", prompt="What does GIL stand for?"))
    assert "fenced" not in prompt
    assert "Answer directly" in prompt


def test_extract_code_from_fence():
    raw = "Here you go:\n```python\ndef solve(a, b):\n    return a + b\n```\n"
    _, code = extract_answer(make_example(), raw)
    assert code == "def solve(a, b):\n    return a + b"


def test_extract_falls_back_to_whole_reply():
    raw = "def solve(a, b):\n    return a + b"
    _, code = extract_answer(make_example(), raw)
    assert code == raw


def test_extract_keeps_qa_answer_verbatim():
    raw = "  Global Interpreter Lock  "
    answer, code = extract_answer(make_example(kind="qa"), raw)
    assert answer == "Global Interpreter Lock"
    assert code == ""


def test_state_roundtrip(tmp_path: Path):
    grant = Grant(request_id="r1", identity_id="i1", examples=[make_example()])
    state = SessionState(
        version=1,
        grant={"request_id": "r1", "identity_id": "i1", "examples": []},
        model_id="m",
        provider="echo",
        started_at=time.time(),
    )
    path = state_path(tmp_path, "r1")
    save_state(path, state)
    loaded = load_state(path)
    assert loaded.model_id == "m"
    assert loaded.grant["request_id"] == "r1"
    assert grant.request_id == "r1"


def test_pending_sessions_excludes_reported(tmp_path: Path):
    for rid, reported in (("a", False), ("b", True)):
        save_state(state_path(tmp_path, rid), SessionState(
            version=1,
            grant={"request_id": rid, "identity_id": "i", "examples": []},
            model_id="m", provider="echo", started_at=time.time(), reported=reported,
        ))
    pending = pending_sessions(tmp_path)
    assert [p.stem for p in pending] == ["a"]


def test_device_payload_never_sends_placeholders():
    payload = device.device_payload()
    for key in ("machine_id", "disk_uuid"):
        if key in payload:
            assert payload[key].strip("0-") != ""
            assert payload[key] not in {"unknown", "none", "default string"}


def test_echo_provider_returns_the_prompt():
    assert Echo().complete("hello world").text == "hello world"


# --------------------------------------------------------- relay client ---


class FakeTransport:
    """Stands in for urlopen, so the client's parsing can be tested alone."""

    def __init__(self, payload: dict, status: int = 200, headers: dict | None = None):
        self.payload = payload
        self.status = status
        self.headers = headers or {}

    def __call__(self, req, timeout=None):
        raise AssertionError("not used directly")


def test_grant_parsing_keeps_only_what_the_relay_sends():
    raw = {
        "request_id": "abc", "identity_id": "def",
        "examples": [{
            "id": "py-1", "category": "python", "difficulty": "easy", "kind": "code",
            "prompt": "do it", "language": "python", "entrypoint": "solve",
        }],
    }
    grant = Grant.from_json(raw)
    ex = grant.examples[0]
    assert ex.prompt == "do it"
    # The relay never sends these; the runner must not invent them either.
    assert not hasattr(ex, "reference_solution")
    assert not hasattr(ex, "tests")


def test_client_rejects_empty_token():
    with pytest.raises(ValueError):
        RelayClient("")


def test_relay_error_marks_rate_limits_as_waitable():
    err = RelayError(status=429, code="WINDOW_ACTIVE", message="wait", retry_after=3600)
    assert err.is_rate_limited
    assert "1 hour" in str(err)


def test_relay_error_distinguishes_identity_problems():
    assert RelayError(403, "PRIVACY_BLOCKED", "").is_identity_problem
    assert not RelayError(429, "WINDOW_ACTIVE", "").is_identity_problem


# --------------------------------------------------------- end to end ---


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _build_relay(tmp_path: Path) -> Path | None:
    """Build the real relay binary, or return None if Go is unavailable."""
    go = shutil.which("go") or "/tmp/gopkg/go/bin/go"
    if not Path(go).exists():
        return None
    out = tmp_path / "relayd"
    env = dict(os.environ)
    env.setdefault("GOROOT", "/tmp/gopkg/go")
    env.setdefault("GOCACHE", "/tmp/gocache")
    env["CGO_ENABLED"] = "0"
    result = subprocess.run(
        [go, "build", "-o", str(out), "./cmd/relayd"],
        cwd=RELAY_DIR, env=env, capture_output=True, text=True,
    )
    if result.returncode != 0:
        return None
    return out


@pytest.fixture(scope="session")
def relay_binary(tmp_path_factory) -> Path:
    tmp = tmp_path_factory.mktemp("build")
    binary = _build_relay(tmp)
    if binary is None:
        pytest.skip("the relay binary could not be built (no Go toolchain)")
    return binary


@pytest.fixture
def relay_factory(relay_binary: Path, tmp_path: Path):
    """Start a fresh relay per test.

    Fresh matters: the relay's state is per identity and per machine, so tests
    sharing one server would trip each other's draw window. Isolating the data
    directory is cheaper and clearer than inventing fake devices.
    """
    started: list[subprocess.Popen] = []

    def start(**overrides: str) -> str:
        port = _free_port()
        data = tmp_path / f"run{len(started)}"
        bank = data / "bank"
        shutil.copytree(RELAY_DIR / "testdata" / "bank", bank)
        env = {
            **os.environ,
            "ADDR": f"127.0.0.1:{port}",
            "DATA_DIR": str(data / "data"),
            "RATE_DIR": str(data / "rate"),
            "BANK_DIR": str(bank),
            "RELAY_SALT": "test-salt-for-the-runner-suite",
            "ACCESS_TOKEN": "test-token",
            "ADMIN_TOKEN": "test-admin",
            "ALLOW_LOOPBACK": "1",
            "LOG_FORMAT": "text",
            # The suite makes several requests per second from one loopback
            # address, which is exactly what the relay's burst ladder exists to
            # stop. That ladder is covered properly by relay/internal/limits;
            # here it would only mask the rules under test. RATE_FLOOR is not
            # usable for this -- the field exists but is never loaded from the
            # environment, so it is zero in production as well.
            "RATE_ALLOWANCE": "100000",
            # No HF sync from a test: the dataset is real and private.
            "HF_DISABLED": "1",
        }
        env.pop("HF_DATA_TOKEN", None)
        env.pop("HF_TOKEN", None)
        env.update(overrides)

        proc = subprocess.Popen([str(relay_binary)], env=env,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        started.append(proc)
        base = f"http://127.0.0.1:{port}"
        deadline = time.time() + 20
        while time.time() < deadline:
            if proc.poll() is not None:
                pytest.fail(f"the relay exited at startup:\n{proc.stdout.read()}")
            try:
                with urllib.request.urlopen(f"{base}/health", timeout=1) as resp:
                    if resp.status == 200:
                        return base
            except (urllib.error.URLError, ConnectionError):
                time.sleep(0.1)
        pytest.fail("the relay did not start in time")

    yield start

    for proc in started:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


@pytest.fixture
def relay_server(relay_factory) -> str:
    """A relay whose draw window is effectively open, for non-window tests."""
    return relay_factory(WINDOW="1ms")


def test_end_to_end_draw_and_report(relay_server: str, tmp_path: Path):
    """The contract that matters: draw, answer, report, accepted."""
    client = RelayClient("test-token", relay_server)

    health = client.health()
    assert health["ok"] is True
    assert health["bank"]["_total"] > 0

    grant = client.draw(size=3)
    assert grant.request_id
    assert grant.identity_id
    assert len(grant.examples) == 3
    assert all(ex.prompt for ex in grant.examples)
    # The runner must never receive these; if it ever does, the answer key leaked.
    for ex in grant.examples:
        assert not hasattr(ex, "reference_solution")
        assert not hasattr(ex, "tests")

    items = [
        {"example_id": ex.id, "category": ex.category, "difficulty": ex.difficulty,
         "answer": "stub", "code": "def solve(*a): return 0", "passed": False, "score": 0}
        for ex in grant.examples
    ]
    response = client.report(grant, items, model_id="test-model")
    assert response["accepted"] is True
    assert response["summary"]["total"] == 3


def test_replay_is_refused(relay_server: str):
    client = RelayClient("test-token", relay_server)
    # A different machine id keeps this test from colliding with the window test.
    grant = client.draw(size=1)
    items = [{"example_id": ex.id, "category": ex.category, "difficulty": ex.difficulty,
              "answer": "x", "passed": False, "score": 0} for ex in grant.examples]
    client.report(grant, items, model_id="m")
    with pytest.raises(RelayError) as info:
        client.report(grant, items, model_id="m")
    assert info.value.code == "ALREADY_REPORTED"


def test_second_draw_in_window_is_refused(relay_factory):
    # The default window, deliberately: this asserts the rule the runner has to
    # live with.
    client = RelayClient("test-token", relay_factory())
    client.draw(size=1)
    with pytest.raises(RelayError) as info:
        client.draw(size=1)
    assert info.value.code == "WINDOW_ACTIVE"
    assert info.value.retry_after is not None
    assert info.value.is_rate_limited


def test_answer_for_a_question_never_issued_is_refused(relay_server: str):
    client = RelayClient("test-token", relay_server)
    grant = client.draw(size=1)
    bogus = [{"example_id": "py-hard-999", "category": "python", "difficulty": "hard",
              "answer": "x", "passed": True, "score": 1}]
    with pytest.raises(RelayError) as info:
        client.report(grant, bogus, model_id="m")
    assert info.value.code == "QUESTION_NOT_ISSUED"


def test_session_survives_interruption(relay_server: str, tmp_path: Path):
    """A killed runner must be resumable, because the draw is already spent."""
    client = RelayClient("test-token", relay_server)
    grant = client.draw(size=2, send_device=False)

    class Flaky:
        name = "flaky"

        def __init__(self):
            self.calls = 0

        def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
            self.calls += 1
            if self.calls == 2:
                raise KeyboardInterrupt
            return Completion(text="```\nanswer\n```")

    # First attempt dies partway through.
    with pytest.raises(KeyboardInterrupt):
        run_session(client, Flaky(), grant, state_dir=tmp_path, model_id="m")

    path = state_path(tmp_path, grant.request_id)
    assert path.exists(), "the draw must be saved before any answer is produced"
    assert load_state(path).items, "completed answers must be persisted as they arrive"

    # Second attempt resumes the same draw rather than drawing again.
    class Steady:
        name = "steady"

        def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
            return Completion(text="```\nanswer\n```")

    state = run_session(client, Steady(), grant, state_dir=tmp_path, model_id="m")
    assert len(state.items) == 2
    response = report_session(client, state, state_dir=tmp_path)
    assert response["accepted"] is True
    assert load_state(path).reported is True


# ------------------------------------------------------- duplicate guard ---


def test_a_repeated_example_is_answered_once(tmp_path: Path):
    """A grant that repeats an id must not produce two answers for it.

    The relay refuses a report containing a duplicate, and that would throw away
    a draw that has already been spent. The runner has to survive the grant
    rather than assume the relay never repeats itself.
    """
    ex = make_example()
    grant = Grant(request_id="r-dup", identity_id="i", examples=[ex, ex])

    class Counter:
        name = "counter"

        def __init__(self):
            self.calls = 0

        def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion:
            self.calls += 1
            return Completion(text="```\ndef solve(a, b):\n    return a + b\n```")

    provider = Counter()
    state = run_session(RelayClient("t", "http://127.0.0.1:1"), provider, grant,
                        state_dir=tmp_path, model_id="m")
    assert provider.calls == 1
    assert len(state.items) == 1


def test_report_dedupes_state_and_marks_partial(tmp_path: Path):
    """State written before the guard existed is cleaned up, not rejected."""
    ex = make_example()
    grant = Grant(request_id="r-old", identity_id="i", examples=[ex])

    state = SessionState(
        version=1,
        grant={"request_id": "r-old", "identity_id": "i",
               "examples": [{"id": ex.id, "category": ex.category,
                             "difficulty": ex.difficulty, "kind": "code",
                             "prompt": ex.prompt}]},
        model_id="m", provider="x", started_at=time.time(),
        items=[
            {"example_id": ex.id, "category": ex.category, "difficulty": ex.difficulty,
             "answer": "", "code": "a", "passed": False, "score": 0.0, "latency_ms": 1},
            {"example_id": ex.id, "category": ex.category, "difficulty": ex.difficulty,
             "answer": "", "code": "b", "passed": False, "score": 0.0, "latency_ms": 1},
        ],
    )
    save_state(state_path(tmp_path, "r-old"), state)

    sent: dict = {}

    class Recorder:
        def report(self, grant, items, *, model_id, partial=False, duration_s=None):
            sent["items"] = items
            sent["partial"] = partial
            return {"accepted": True}

    report_session(Recorder(), state, state_dir=tmp_path)
    assert len(sent["items"]) == 1
    assert sent["partial"] is True, "a repaired report must say it is partial"


def test_report_is_partial_when_an_item_errored(tmp_path: Path):
    ex = make_example()
    state = SessionState(
        version=1,
        grant={"request_id": "r-err", "identity_id": "i",
               "examples": [{"id": ex.id, "category": ex.category,
                             "difficulty": ex.difficulty, "kind": "code",
                             "prompt": ex.prompt}]},
        model_id="m", provider="x", started_at=time.time(),
        items=[{"example_id": ex.id, "category": ex.category, "difficulty": ex.difficulty,
                "answer": "", "code": "", "passed": False, "score": 0.0,
                "latency_ms": 5, "error": "provider timed out"}],
    )
    sent: dict = {}

    class Recorder:
        def report(self, grant, items, *, model_id, partial=False, duration_s=None):
            sent["partial"] = partial
            return {"accepted": True}

    report_session(Recorder(), state, state_dir=tmp_path)
    assert sent["partial"] is True


def test_draw_without_categories_asks_the_bank(tmp_path: Path):
    """The relay refuses a category-less draw; the runner should not send one."""
    calls: list[str] = []

    class Stub(RelayClient):
        def __init__(self):
            super().__init__("t", "http://127.0.0.1:1")

        def categories(self):
            return ["logic", "python"]

        def _post(self, path, body, *, authenticated):
            calls.append(json.dumps(body))
            return {"request_id": "r", "identity_id": "i", "examples": []}

    grant = Stub().draw(size=2, send_device=False)
    assert grant.request_id == "r"
    assert json.loads(calls[0])["categories"] == ["logic", "python"]


# ---------------------------------------------------------- device block ---


def test_chassis_codes_are_read_as_classes():
    # 23/25/28/29 are rack, blade and enclosure chassis types.
    assert device._server_chassis("23") == "server"
    assert device._server_chassis("28") == "server"
    assert device._server_chassis("10") == "desktop"
    assert device._server_chassis("") == ""


def test_unobservable_fields_are_omitted_not_blank():
    """An empty string would be a value the relay has to interpret."""
    payload = device.device_payload()
    assert "" not in payload.values()
    assert "device_class" not in payload or payload["device_class"] in {"server", "desktop"}
    # Sent only when true: false is a claim this code cannot make on macOS.
    assert "hypervisor" not in payload or payload["hypervisor"] is True
