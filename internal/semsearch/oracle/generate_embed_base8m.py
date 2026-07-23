# /// script
# requires-python = "==3.13.*"
# dependencies = [
#   "huggingface-hub==1.24.0",
#   "model2vec==0.8.2",
#   "numpy==2.5.1",
#   "safetensors==0.8.0",
#   "tokenizers==0.23.1",
# ]
# ///

import hashlib
import json
from importlib.metadata import version
from pathlib import Path

import numpy as np
from huggingface_hub import snapshot_download
from model2vec import StaticModel


ORACLE_DIR = Path(__file__).resolve().parent
EMBED_TESTDATA_DIR = ORACLE_DIR.parent / "embed" / "testdata"
SOURCE_GOLDEN_PATH = EMBED_TESTDATA_DIR / "golden.json"
OUTPUT_PATH = EMBED_TESTDATA_DIR / "golden_base8m.json"

MODEL2VEC_VERSION = "0.8.2"
MODEL_REPO = "minishlab/potion-base-8M"
MODEL_REVISION = "bf8b056651a2c21b8d2565580b8569da283cab23"
MODEL_FILES = ("config.json", "tokenizer.json", "model.safetensors")


def float32_vector(vector: np.ndarray) -> list[float]:
    return [float(value) for value in np.asarray(vector, dtype=np.float32)]


def sha256(path: Path) -> str:
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def main() -> None:
    installed = version("model2vec")
    if installed != MODEL2VEC_VERSION:
        raise RuntimeError(
            f"installed model2vec {installed} does not match pinned {MODEL2VEC_VERSION}"
        )

    source = json.loads(SOURCE_GOLDEN_PATH.read_text(encoding="utf-8"))
    texts = source["texts"]
    snapshot = Path(snapshot_download(MODEL_REPO, revision=MODEL_REVISION))
    model = StaticModel.from_pretrained(str(snapshot), normalize=True)
    vectors = [
        float32_vector(
            model.encode(
                [text],
                max_length=None,
                batch_size=1,
                use_multiprocessing=False,
            )[0]
        )
        for text in texts
    ]
    payload = {
        "dims": int(model.dim),
        "texts": texts,
        "vectors": vectors,
    }
    encoded = json.dumps(
        payload,
        ensure_ascii=False,
        allow_nan=False,
        indent=2,
        sort_keys=True,
    )
    OUTPUT_PATH.write_text(f"{encoded}\n", encoding="utf-8")

    repo_root = ORACLE_DIR.parents[2]
    print(
        json.dumps(
            {
                "dims": model.dim,
                "fixture": str(OUTPUT_PATH.relative_to(repo_root)),
                "sha256": {
                    name: sha256(snapshot / name)
                    for name in MODEL_FILES
                },
            },
            indent=2,
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()
