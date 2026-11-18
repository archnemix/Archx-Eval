"""Archx-Eval front runner.

Runs a draw of questions against a model you control, and reports what the model
answered. It does not grade: the answer key never leaves the relay, so scoring
happens on the server side where that key lives.

    pip install archx-eval
    archx-eval run --provider ollama --model llama3 --categories python

The runner is a library and a command line tool. It deliberately exposes no tool
or agent interface: a runner that an evaluated agent could invoke would be
handing the model its own questions and its own draw, which defeats the point.
"""

__version__ = "0.1.0"
