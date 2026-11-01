import argparse
import json
from pathlib import Path
import re
import subprocess
import sys


DEFAULT_REPO = "/Users/yasyf/.orca/workspaces/monorepo/monorepo/amberjack"
DEFAULT_DRIVE = "durable-agents-rebuild"
DEFAULT_CURSOR = "/Users/yasyf/.claude/scratch/durable-agents-rebuild/drive-watchdog/headlabel.internal-seq.cursor"
DEFAULT_SEQUENCE = 147940
HEAD_KEY = re.compile(r"head:(?:[\w.-]+/[\w.-]+#)?([1-9][0-9]*)", re.ASCII)
HEAD_VALUE = re.compile(r"(?:([0-9a-f]{7,40})\.\.)?([0-9a-f]{7,40})(?: (.*))?")
IDENTICAL = re.compile(r"patch-identical \(([0-9]+) commits?\)")
LEGACY = re.compile(r"HEAD #([0-9]+) ([0-9a-f]{40}) \(was ([0-9a-f]{40})\): .*patch-identical \(([0-9]+) commits?\)")
BAD_PAIR = re.compile(r"\s*(\d+|-):\s+([0-9a-f]+|-+)\s+[<>!]")


def arguments(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", default=DEFAULT_REPO)
    parser.add_argument("--drive", default=DEFAULT_DRIVE)
    parser.add_argument("--cursor", type=Path, default=Path(DEFAULT_CURSOR))
    parser.add_argument("--initial-seq", type=int, default=DEFAULT_SEQUENCE)
    return parser.parse_args(argv)


def claims(record):
    if record["kind"] != "head":
        return []
    heads = [(key, value) for key, value in record.get("fields", {}).get("attrs", {}).items() if key.startswith("head:")]
    if not heads:
        match = LEGACY.search(f"HEAD {record['lane']} {record['text']}")
        return [match.groups()] if match else []
    result = []
    for key, value in sorted(heads):
        pr = HEAD_KEY.fullmatch(key)
        moved = HEAD_VALUE.fullmatch(value)
        if pr is None or moved is None:
            raise ValueError(f"malformed structured HEAD {key}: {value!r}")
        old, new, note = moved.groups()
        note = note or ""
        if "patch-identical" not in note:
            continue
        counts = list(IDENTICAL.finditer(note))
        if old is None or len(counts) != 1 or note.count("patch-identical") != 1:
            raise ValueError(f"ambiguous structured HEAD {key}: {value!r}")
        result.append((pr.group(1), new, old, counts[0].group(1)))
    return result


def range_diff(repo, new, old, count, local):
    argv = ["git"]
    if local:
        argv.append("--no-lazy-fetch")
    return subprocess.run(
        [*argv, "-C", str(repo), "range-diff", f"{old}~{count}..{old}", f"{new}~{count}..{new}"],
        capture_output=True,
        text=True,
    )


def compare(repo, new, old, count):
    local = range_diff(repo, new, old, count, True)
    if local.returncode == 0:
        return local.stdout
    subprocess.run(
        ["git", "-C", str(repo), "fetch", "-q", "origin", new, old],
        capture_output=True,
        text=True,
        timeout=45,
        check=True,
    )
    fetched = range_diff(repo, new, old, count, False)
    fetched.check_returncode()
    return fetched.stdout


def audit(args):
    since = int(args.cursor.read_text().strip()) if args.cursor.exists() else args.initial_seq
    if since < 0:
        raise ValueError("the cursor must be a nonnegative internal sequence")
    tail = subprocess.run(
        ["cci", "tail", "--drive", args.drive, "--since-seq", str(since), "--kind", "head", "--json", "--budget", "40000"],
        capture_output=True,
        text=True,
        cwd="/tmp",
        check=True,
    )
    if tail.stderr:
        print(tail.stderr, end="", file=sys.stderr)
    top = since
    try:
        for line in tail.stdout.splitlines():
            record = json.loads(line)
            sequence = record["seq"]
            if type(sequence) is not int or sequence <= top:
                raise ValueError(f"CCI returned an unordered internal sequence: {sequence!r}")
            messages = []
            for pr, new, old, count in claims(record):
                output = compare(args.repo, new, old, count)
                bad = [line for line in output.splitlines() if BAD_PAIR.match(line)]
                if bad:
                    name = record.get("num") or f"#{sequence}"
                    rendered = " | ".join(line.strip()[:140] for line in bad[:4])
                    messages.append(f"HEAD-MISLABEL #{pr} {new[:7]} (record {name}) says patch-identical; range-diff: {rendered}")
            for message in messages:
                print(message)
            top = sequence
    finally:
        if top != since:
            args.cursor.write_text(str(top))


def main(argv=None):
    try:
        audit(arguments(argv))
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        print(f"headlabel-check: {error}", file=sys.stderr)
        if error.stderr:
            print(error.stderr, end="", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
