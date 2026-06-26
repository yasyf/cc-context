"""Token counting via the Anthropic count-tokens API, cached on disk.

The count-tokens API is the ground truth for "how many tokens does this text cost
Claude." Counts are stable across a model family, so a single cheap model is used
as the counter. Every distinct (model, text) result is memoized to a content-hash
file so reruns and the highly repetitive bench corpus never re-call the network.
"""

from __future__ import annotations

import hashlib
import threading
from pathlib import Path
from typing import Callable

DEFAULT_MODEL = "claude-haiku-4-5-20251001"
DEFAULT_CACHE_DIR = Path(__file__).resolve().parent.parent / ".token-cache"

CountFn = Callable[[str, str], int]


def api_count(text: str, model: str) -> int:
    """Count tokens for `text` as a single user message via the Anthropic API."""
    import anthropic

    client = anthropic.Anthropic()
    resp = client.messages.count_tokens(
        model=model,
        messages=[{"role": "user", "content": text or " "}],
    )
    return int(resp.input_tokens)


class TokenCounter:
    """Counts tokens through `count_fn`, memoizing every result under `cache_dir`.

    `count_fn` is the only external boundary; tests inject a deterministic fake so
    no network call happens.
    """

    def __init__(
        self,
        *,
        model: str = DEFAULT_MODEL,
        count_fn: CountFn = api_count,
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
    """Return the process-wide cached counter, building it on first use."""
    global _default
    with _default_lock:
        if _default is None:
            _default = TokenCounter()
        return _default
