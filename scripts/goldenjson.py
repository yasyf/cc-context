#!/usr/bin/env python3
"""Writes one recorded scenario as a single JSON container.

Recorded payloads become JSON strings, which is what keeps the repo's
formatting hooks off bytes a real tool produced: `json.dumps` escapes every
newline, so a payload occupies exactly one line of the container and no
captured byte can ever sit at end of line, where `trailing-whitespace` would
strip it. `end-of-file-fixer` can only touch the container's own final
newline, which this writer already emits exactly one of, so both hooks are
no-ops on what it writes.
"""

import argparse
import json
import pathlib
import sys

ORDER = ("argv", "exit", "status", "headers", "stdout", "body", "stderr")


def read_text(path):
    return pathlib.Path(path).read_bytes().decode()


def read_lines(path):
    raw = read_text(path)
    if raw == "":
        return []
    return raw.removesuffix("\n").split("\n")


def encode(fields):
    ordered = {key: fields[key] for key in ORDER if key in fields}
    return json.dumps(ordered, indent=2, ensure_ascii=False) + "\n"


def pair(spec):
    key, sep, value = spec.partition("=")
    if not sep:
        raise argparse.ArgumentTypeError(f"want KEY=VALUE, got {spec!r}")
    return key, value


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("out")
    parser.add_argument("--text", type=pair, action="append", default=[], metavar="KEY=PATH")
    parser.add_argument("--lines", type=pair, action="append", default=[], metavar="KEY=PATH")
    parser.add_argument("--int", type=pair, action="append", default=[], metavar="KEY=VALUE", dest="ints")
    parser.add_argument("--argv", nargs=argparse.REMAINDER, default=[])
    args = parser.parse_args()

    fields = {}
    for key, value in args.ints:
        fields[key] = int(value)
    for key, path in args.text:
        fields[key] = read_text(path)
    for key, path in args.lines:
        fields[key] = read_lines(path)
    if args.argv:
        fields["argv"] = args.argv

    unknown = set(fields) - set(ORDER)
    if unknown:
        print(f"goldenjson: unknown field(s) {sorted(unknown)}", file=sys.stderr)
        return 2

    out = pathlib.Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(encode(fields), encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
