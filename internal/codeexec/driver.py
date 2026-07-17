"""cc-context codeexec driver.

Speaks JSON Lines with the Go host: an init frame arrives on stdin, call
frames go out on the protocol stream, result frames come back on stdin, and
one done frame ends the run. Exit codes: 0 done emitted (even for compile,
typecheck, and run failures), 1 driver crash (traceback on stderr), 2 stdin
EOF.
"""

import asyncio
import json
import math
import os
import re
import sys
import threading
import traceback

# Protect the protocol before anything else can touch fd 1: keep a private
# handle on the real stdout, then point fd 1 at devnull so a stray native
# write cannot corrupt the frame stream.
PROTO = os.fdopen(os.dup(1), "w", buffering=1)
_devnull = os.open(os.devnull, os.O_WRONLY)
os.dup2(_devnull, 1)
os.close(_devnull)

from pydantic_monty import Monty, MontyRuntimeError, MontySyntaxError, MontyTypingError

ARG_CEILING = 64 << 20
MAX_DEPTH = 64  # wire nesting cap, mirrored in value.go maxWireDepth
STDOUT_CAP = 8 << 20  # print noise truncates; the marker names the cut
VALUE_CAP = 32 << 20  # encoded final value; truncating one would corrupt it, so over-cap errors

# Matches the [ccx:CODE] wire tag the host-result site prefixes onto a raised
# error; the terminal handler extracts the code and strips the tag (and one
# trailing space) from the reported message.
CODE_TAG = re.compile(r"\[ccx:([a-z_]+)\] ?")

# Lambdas resolve decode at call time, so the table can precede its definition.
TAGS = {
    "$tuple": lambda v, d: tuple(decode(x, d) for x in v),
    "$set": lambda v, d: {decode(x, d) for x in v},
    "$frozenset": lambda v, d: frozenset(decode(x, d) for x in v),
    "$bytes": lambda v, d: bytes(v),
    "$float": lambda v, d: float(v),
    "$dict": lambda v, d: {decode(k, d): decode(x, d) for k, x in v},
}


def send(frame):
    PROTO.write(json.dumps(frame, separators=(",", ":")) + "\n")


def finish(frame):
    send(frame)
    PROTO.flush()
    os._exit(0)


def crash():
    traceback.print_exc()
    sys.stderr.flush()
    os._exit(1)


def encode(v, depth=0):
    if depth > MAX_DEPTH:
        raise RuntimeError(f"value nesting exceeds depth {MAX_DEPTH}")
    if v is None or isinstance(v, (bool, int, str)):
        return v
    if isinstance(v, float):
        if math.isnan(v):
            return {"$float": "nan"}
        if math.isinf(v):
            return {"$float": "inf" if v > 0 else "-inf"}
        return v
    if isinstance(v, bytes):
        return {"$bytes": list(v)}
    if isinstance(v, tuple):
        return {"$tuple": [encode(x, depth + 1) for x in v]}
    if isinstance(v, set):
        return {"$set": [encode(x, depth + 1) for x in v]}
    if isinstance(v, frozenset):
        return {"$frozenset": [encode(x, depth + 1) for x in v]}
    if isinstance(v, list):
        return [encode(x, depth + 1) for x in v]
    if isinstance(v, dict):
        if all(isinstance(k, str) for k in v):
            if len(v) == 1:
                ((k, x),) = v.items()
                # A lone $-prefixed key is tag-shaped in-band data: escape it
                # as $dict so the decoders round-trip it as a dict, not a tag.
                if k.startswith("$"):
                    return {"$dict": [[k, encode(x, depth + 1)]]}
            return {k: encode(x, depth + 1) for k, x in v.items()}
        return {"$dict": [[encode(k, depth + 1), encode(x, depth + 1)] for k, x in v.items()]}
    raise RuntimeError(f"unencodable sandbox value: {type(v).__name__}")


def decode(v, depth=0):
    if depth > MAX_DEPTH:
        raise RuntimeError(f"value nesting exceeds depth {MAX_DEPTH}")
    if isinstance(v, list):
        return [decode(x, depth + 1) for x in v]
    if isinstance(v, dict):
        if len(v) == 1:
            ((key, val),) = v.items()
            if key in TAGS:
                return TAGS[key](val, depth + 1)
        return {k: decode(x, depth + 1) for k, x in v.items()}
    return v


class Host:
    """Bridges sandbox externals to the Go host over the frame stream.

    One process-lifetime reader thread owns stdin from construction on: EOF
    at any phase — even mid compile or typecheck — is the host's kill signal
    (exit 2), and run-phase result lines route onto the asyncio loop. The
    reader never takes the write lock, so a stalled call can't stall its
    answer.
    """

    def __init__(self, loop):
        self.loop = loop
        self.lock = asyncio.Lock()
        self.futures = {}
        self.last_id = 0
        threading.Thread(target=self.read_results, daemon=True).start()

    def external(self, name):
        async def call(*args, **kwargs):
            body = {
                "fn": name,
                "args": [encode(a) for a in args],
                "kwargs": {k: encode(v) for k, v in kwargs.items()},
            }
            if len(json.dumps(body, separators=(",", ":"))) > ARG_CEILING:
                raise RuntimeError(
                    f"codeexec: arguments to {name} exceed {ARG_CEILING} bytes encoded; pass less data per call"
                )
            fut = self.loop.create_future()
            async with self.lock:
                self.last_id += 1
                self.futures[self.last_id] = fut
                send({"t": "call", "id": self.last_id, **body})
            result = await fut
            if not result.get("ok"):
                message = result.get("error") or f"host call {name} failed"
                # [ccx:CODE] is a wire tag driver.py both writes here and reads
                # at the terminal handler — not cross-boundary string-matching.
                # Known leak: a script that stringifies the caught error sees it.
                code = result.get("err_code")
                if code:
                    message = f"[ccx:{code}] {message}"
                raise RuntimeError(message)
            return decode(result.get("value"))

        return call

    def read_results(self):
        while True:
            line = sys.stdin.readline()
            if not line:
                os._exit(2)
            try:
                frame = json.loads(line)
                self.loop.call_soon_threadsafe(self.resolve, frame)
            except BaseException:
                crash()

    def resolve(self, frame):
        try:
            self.futures.pop(frame["id"]).set_result(frame)
        except BaseException:
            crash()


async def main():
    init = json.loads(sys.stdin.readline())
    host = Host(asyncio.get_running_loop())
    send({"t": "ready"})
    try:
        monty = Monty(init["script"], script_name="exec.py")
    except MontySyntaxError as e:
        finish({"t": "done", "ok": False, "phase": "compile", "error": e.display("traceback")})
    try:
        monty.type_check(type_check_stubs=init["stubs"])
    except MontyTypingError as e:
        finish({"t": "done", "ok": False, "phase": "typecheck", "error": e.display("full", False)})

    stdout = []
    collected = 0

    def collect(stream, text):
        # Chunks are sliced to the remaining budget so one giant print cannot
        # balloon retained memory; the emit-side slice makes the cut exact.
        nonlocal collected
        if collected > STDOUT_CAP:
            return
        keep = text[: STDOUT_CAP + 1 - collected]
        stdout.append(keep)
        collected += len(keep)

    try:
        out = await monty.run_async(
            external_functions={name: host.external(name) for name in init["functions"]},
            limits=init["limits"],
            print_callback=collect,
        )
    except MontyRuntimeError as e:
        message = str(e)
        frame = {"t": "done", "ok": False, "phase": "run", "error": message}
        m = CODE_TAG.search(message)
        if m:
            frame["err_code"] = m.group(1)
            frame["error"] = CODE_TAG.sub("", message, count=1)
        finish(frame)
    except BaseException:
        crash()
    try:
        value = encode(out)
        if len(json.dumps(value, separators=(",", ":"))) > VALUE_CAP:
            finish(
                {
                    "t": "done",
                    "ok": False,
                    "phase": "run",
                    "error": "final value exceeds 32 MiB; return a narrower value",
                }
            )
    except Exception as e:
        finish({"t": "done", "ok": False, "phase": "run", "error": f"{type(e).__name__}: {e}"})
    text = "".join(stdout)
    if len(text) > STDOUT_CAP:
        text = text[:STDOUT_CAP] + "\n[stdout truncated at 8 MiB]"
    finish({"t": "done", "ok": True, "value": value, "stdout": text})


try:
    asyncio.run(main())
except BaseException:
    crash()
