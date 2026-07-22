from __future__ import annotations

import json
import os
from dataclasses import dataclass
from importlib.metadata import version
from pathlib import Path
from typing import Any
from unittest.mock import patch

import numpy as np
import numpy.typing as npt
from huggingface_hub import snapshot_download
from model2vec import StaticModel
from semble import SembleIndex
from semble.index.bm25 import BM25
from semble.index.create import create_index_from_path
from semble.index.dense import SelectableBasicBackend, embed_chunks, load_model
from semble.types import Chunk, ContentType


ORACLE_DIR = Path(__file__).resolve().parent
TESTDATA_DIR = ORACLE_DIR.parent / "testdata"
CORPUS_DIR = TESTDATA_DIR / "corpus"
GOLDENS_DIR = TESTDATA_DIR / "goldens"
PINS_PATH = TESTDATA_DIR / "pins.json"
QUERIES_PATH = TESTDATA_DIR / "queries.json"


@dataclass(frozen=True)
class OracleIndex:
    model: StaticModel
    model_path: str
    bm25: BM25
    semantic: SelectableBasicBackend
    chunks: list[Chunk]

    def public_index(self) -> SembleIndex:
        return SembleIndex(
            self.model,
            self.bm25,
            self.semantic,
            self.chunks,
            self.model_path,
            content=(ContentType.CODE, ContentType.DOCS, ContentType.CONFIG),
        )


def require_deterministic_hash_seed() -> None:
    if os.environ.get("PYTHONHASHSEED") != "0":
        raise RuntimeError("run oracle scripts with PYTHONHASHSEED=0")


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def load_pins() -> dict[str, str]:
    pins = read_json(PINS_PATH)
    installed = version("semble")
    if installed != pins["semble_version"]:
        raise RuntimeError(f"installed semble {installed} does not match pinned {pins['semble_version']}")
    return pins


def load_queries() -> dict[str, Any]:
    return read_json(QUERIES_PATH)


def resolve_model_snapshot() -> str:
    pins = load_pins()
    return snapshot_download(
        repo_id=pins["model_repo"],
        revision=pins["model_revision"],
    )


def embed_chunks_padding_free(model: StaticModel, chunks: list[Chunk]) -> npt.NDArray[np.float32]:
    return np.concatenate([embed_chunks(model, [chunk]) for chunk in chunks])


def load_oracle_index() -> OracleIndex:
    require_deterministic_hash_seed()
    snapshot = resolve_model_snapshot()
    model, _ = load_model(snapshot)
    # Singleton batches keep unmasked padding out of model2vec's mean pool.
    with patch("semble.index.create.embed_chunks", embed_chunks_padding_free):
        bm25, semantic, chunks, _ = create_index_from_path(
            CORPUS_DIR,
            model,
            content=(ContentType.CODE, ContentType.DOCS, ContentType.CONFIG),
            display_root=CORPUS_DIR,
        )
    return OracleIndex(
        model=model,
        model_path=snapshot,
        bm25=bm25,
        semantic=semantic,
        chunks=chunks,
    )


def chunk_location(chunk: Chunk) -> dict[str, str | int]:
    return {
        "file_path": chunk.file_path,
        "start_line": chunk.start_line,
        "end_line": chunk.end_line,
    }


def write_golden(name: str, payload: Any) -> None:
    GOLDENS_DIR.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(
        payload,
        ensure_ascii=False,
        allow_nan=False,
        indent=2,
        sort_keys=True,
    )
    (GOLDENS_DIR / name).write_text(f"{encoded}\n", encoding="utf-8")
