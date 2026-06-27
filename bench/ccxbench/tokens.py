"""Token counting, cached on disk.

By default counts run through a local tiktoken proxy (`local_count`) — no network and
no API key, which matters because the rest of the bench authenticates Claude through
spawnllm's Claude Code OAuth, never an `ANTHROPIC_API_KEY`. The proxy is exact enough for
how counts are used: the headline high-water mark comes straight from transcript usage and
needs no tokenizer, and every place a count is used (decomposition buckets, the Layer-1
micro-bench) it sits inside an arm-vs-arm ratio where a proxy's systematic error cancels.

The Anthropic count-tokens API is kept as an opt-in `count_fn` for a high-fidelity run when
an `ANTHROPIC_API_KEY` is present. Distinct (model, text, counter) results are memoized to a
content-hash file so reruns and the highly repetitive corpus never recompute.
"""

from __future__ import annotations

import hashlib
import threading
from pathlib import Path
from typing import Callable

DEFAULT_MODEL = "claude-haiku-4-5-20251001"
DEFAULT_CACHE_DIR = Path(__file__).resolve().parent.parent / ".token-cache"
TIKTOKEN_ENCODING = "cl100k_base"

CountFn = Callable[[str, str], int]

_encoders: dict[str, object] = {}
_encoder_lock = threading.Lock()


def _encoder():
    import tiktoken

    with _encoder_lock:
        enc = _encoders.get(TIKTOKEN_ENCODING)
        if enc is None:
            enc = tiktoken.get_encoding(TIKTOKEN_ENCODING)
            _encoders[TIKTOKEN_ENCODING] = enc
        return enc


def local_count(text: str, model: str) -> int:
    """Count tokens with a local tiktoken proxy — no network, no API key."""
    return len(_encoder().encode(text or " "))


def api_count(text: str, model: str) -> int:
    """Count tokens via the Anthropic count-tokens API (needs ANTHROPIC_API_KEY)."""
    import anthropic

    client = anthropic.Anthropic()
    resp = client.messages.count_tokens(
        model=model,
        messages=[{"role": "user", "content": text or " "}],
    )
    return int(resp.input_tokens)


class TokenCounter:
    """Counts tokens through `count_fn`, memoizing every result under `cache_dir`.

    Defaults to the offline `local_count`; pass `count_fn=api_count` for a high-fidelity
    run. `count_fn` is the only external boundary; tests inject a deterministic fake.
    """

    def __init__(
        self,
        *,
        model: str = DEFAULT_MODEL,
        count_fn: CountFn = local_count,
        cache_dir: Path = DEFAULT_CACHE_DIR,
    ) -> None:
        self.model = model
        self._count_fn = count_fn
        self._cache_dir = cache_dir
        self._mem: dict[str, int] = {}
        self._lock = threading.Lock()
        self._cache_dir.mkdir(parents=True, exist_ok=True)

    def _key(self, text: str) -> str:
        h = hashlib.sha256()
        h.update(self.model.encode())
        h.update(b"\x00")
        h.update(getattr(self._count_fn, "__name__", "fn").encode())
        h.update(b"\x00")
        h.update(text.encode())
        return h.hexdigest()

    def count(self, text: str) -> int:
        key = self._key(text)
        with self._lock:
            if key in self._mem:
                return self._mem[key]
        path = self._cache_dir / key
        if path.exists():
            value = int(path.read_text())
            with self._lock:
                self._mem[key] = value
            return value
        value = self._count_fn(text, self.model)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(str(value))
        tmp.replace(path)
        with self._lock:
            self._mem[key] = value
        return value


_default: TokenCounter | None = None
_default_lock = threading.Lock()


def default_counter() -> TokenCounter:
    """Return the process-wide cached counter (offline tiktoken), building it on first use."""
    global _default
    with _default_lock:
        if _default is None:
            _default = TokenCounter()
        return _default
