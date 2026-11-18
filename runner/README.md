# archx-eval runner

Draws questions from an Archx-Eval relay, asks a model, reports the answers.

This is the client half of the eval. It is a **library and a CLI that a human
runs**. There is deliberately no agent harness, no tool-use loop and no MCP
server: anything that can call this and also see the answer key could cheat, and
a runner that grades itself has the same problem, so the answer key never
reaches this code at all. The relay strips `reference_solution`, `tests`,
`expected_answer` and `rubric` before a grant is sent, and nothing here scores
anything — it records what the model said and files it.

## Install

```bash
pip install -e ./runner            # from the repo root
pip install -e './runner[gguf]'    # plus llama-cpp-python for --provider gguf
```

## Use

```bash
export ARCHX_ACCESS_TOKEN=...                 # the relay's ACCESS_TOKEN
export ARCHX_BASE_URL=https://api-relay-archx-eval.hf.space

archx-eval doctor                             # reachable? how much stock?
archx-eval run --provider ollama --model llama3.2 --size 10
archx-eval status                             # draws still waiting to be reported
archx-eval resume                             # finish one that was interrupted
```

A draw is expensive: one identity may draw once per window, so the runner saves
the grant to disk the moment it arrives and saves each answer as it is produced.
If the process dies you lose at most one question's work, and `resume` files the
run instead of wasting the window.

Default state directory is `~/.local/state/archx-eval/`; override with
`--state-dir`.

Exit codes are stable, so the CLI can sit in a script:

| code | meaning |
| ---- | ------- |
| 0 | fine |
| 1 | usage or configuration problem |
| 2 | the relay refused (rate limit, ban, bad token, …) |
| 3 | the model provider failed |

## Providers

| `--provider` | what it talks to | notes |
| ------------ | ---------------- | ----- |
| `openai` | anything OpenAI-compatible: OpenAI, vLLM, LM Studio, llama.cpp server, OpenRouter, Together | `--provider-url` for a local server |
| `anthropic` | Anthropic Messages API | `ANTHROPIC_API_KEY` |
| `gemini` | Google Generative Language API | `GEMINI_API_KEY` |
| `ollama` | a local Ollama daemon | no key needed |
| `gguf` | a `.gguf` file loaded in-process | needs `[gguf]` extra; `--model /path/to.gguf` |
| `echo` | nothing at all | plumbing test; echoes the prompt |

Local endpoints work through the same switch:

```bash
archx-eval run --provider ollama --model qwen2.5-coder
archx-eval run --provider openai --provider-url http://localhost:1234/v1 --model local-model
archx-eval run --provider gguf --model ./models/qwen2.5-7b-instruct-q4_k_m.gguf
```

API keys are read from the usual environment variables (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`) or `--api-key`.

## Library

```python
from archx_eval.relay import RelayClient
from archx_eval.providers import ProviderConfig
from archx_eval.session import run_session, report_session

client = RelayClient(token="...", base_url="https://api-relay-archx-eval.hf.space")
grant = client.draw(size=10, categories=["python"])

provider = ProviderConfig(provider="ollama", model="qwen2.5-coder").build()
state = run_session(client, provider, grant, state_dir=path, model_id="qwen2.5-coder")
response = report_session(client, state, state_dir=path)
```

Every provider implements one method:

```python
def complete(self, prompt: str, *, max_tokens: int = 1024) -> Completion: ...
```

so a custom backend is a small class. Anything that raises `ProviderError` is
recorded against that one question; the run continues and the report is filed as
partial rather than pretending the question was answered.

## What the relay will refuse

These are normal answers, not crashes, and each gets a plain explanation:

| code | meaning |
| ---- | ------- |
| `WINDOW_ACTIVE` | this identity already drew in the current window; wait |
| `IDENTITY_BANNED` | a ban is being served; escalates if repeated |
| `NETWORK_BANNED` / `NETWORK_CEILING` | this subnet is banned / at its draw ceiling |
| `GLOBAL_CEILING` | the relay is at capacity |
| `SPAM_RATE` | requests arrived faster than allowed |
| `PRIVACY_BLOCKED` | VPN, Tor or hosting ranges are refused by design |
| `ACCESS_DENIED` | wrong `ACCESS_TOKEN` |
| `UNKNOWN_REQUEST`, `REPORT_TOO_LATE` | the draw is too old to report |
| `ALREADY_REPORTED` | these results were accepted once already |
| `QUESTION_NOT_ISSUED` | the report names a question the grant did not contain |

The runner never retries a draw on its own. A retry cannot succeed inside the
window and only moves the identity up the rate ladder.

## Identity

The runner sends hashed markers so the relay can tell one machine from another
without storing anything raw: `machine_id`, `disk_uuid`, plus OS, architecture,
Python version, device class and clock skew. Use `--no-device` to send none of
it — the relay then identifies the caller only by network, which is weaker and
much more likely to collide with someone else on the same connection.

Nothing here is a security boundary. `ACCESS_TOKEN` keeps crawlers out, the
rate ladder keeps floods out, and both are best effort by design.

## Tests

```bash
cd runner && python -m pytest -q
```

The suite includes an end-to-end test that builds the real relay binary, draws
from it, answers with a fake provider and reports the result. It skips cleanly
when no Go toolchain is available; the rest of the suite has no network and no
Go dependency.
