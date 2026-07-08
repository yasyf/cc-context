"""cc-context web embedding driver.

One request per process, no host calls: a single JSON object arrives on the
first stdin line — {"model": "<hf-repo>@<revision>", "texts": [...]} — and one
JSON response leaves on stdout: {"dims": N, "vectors": [[...]]}, every vector
L2-normalized. Any failure tracebacks to stderr and exits 1; stdin EOF is the
host's kill signal and exits 2.
"""

import json
import os
import sys
import threading

# Protect the response stream before anything else can touch fd 1: keep a
# private handle on the real stdout, then point fd 1 at devnull so a stray
# library print cannot corrupt the response.
PROTO = os.fdopen(os.dup(1), "w")
_devnull = os.open(os.devnull, os.O_WRONLY)
os.dup2(_devnull, 1)
os.close(_devnull)

request = json.loads(sys.stdin.readline())


def watch_stdin():
    while sys.stdin.readline():
        pass
    os._exit(2)


# The host keeps stdin open for the process lifetime; EOF at any phase — even
# mid model-download — means the host is gone, and only this thread can notice
# (killing uv alone would orphan this python grandchild).
threading.Thread(target=watch_stdin, daemon=True).start()

from huggingface_hub import snapshot_download
from model2vec import StaticModel

repo, _, revision = request["model"].partition("@")
if not revision:
    raise ValueError(f"model {request['model']!r} carries no @revision pin")

# snapshot_download pins the exact revision and is cache-aware (a cached
# commit-sha snapshot never touches the network). StaticModel.from_pretrained
# has no revision parameter and force-downloads by default (verified against
# model2vec 0.8.2), so it only ever sees the local snapshot path.
# normalize=True guarantees L2-normalized output regardless of model config.
model = StaticModel.from_pretrained(snapshot_download(repo, revision=revision), normalize=True)

# max_length=None: chunks may exceed the 512-token default and a static model
# has no context limit, so never truncate. use_multiprocessing=False: forked
# workers would outlive the host's kill.
vectors = model.encode(request["texts"], max_length=None, use_multiprocessing=False)

json.dump({"dims": int(vectors.shape[1]), "vectors": vectors.tolist()}, PROTO)
PROTO.flush()
