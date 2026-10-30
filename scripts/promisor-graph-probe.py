import collections
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

OUT = Path(os.environ["PROBE_OUTPUT"])
OUT.mkdir(parents=True, exist_ok=True)
ROOT = Path(tempfile.mkdtemp(prefix="promisor-graph-"))
GIT = shutil.which("git")
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_TERMINAL_PROMPT="0", GIT_NO_LAZY_FETCH="1", GIT_AUTHOR_NAME="Probe", GIT_AUTHOR_EMAIL="probe@example.invalid", GIT_COMMITTER_NAME="Probe", GIT_COMMITTER_EMAIL="probe@example.invalid", GIT_AUTHOR_DATE="1700000000 +0000", GIT_COMMITTER_DATE="1700000000 +0000")
REPORT = {"git": subprocess.check_output([GIT, "version"], env=ENV, text=True).strip(), "rows": [], "limits": ["Native unpack_entry trace counts logical unpack operations, not physical throughput", "Pack mmap/read syscall counters are separate from per-type unpack counts", "Blob-body skip already present in Git2.56", "No production changes or Git patch"]}
assert REPORT["git"] == "git version 2.56.0"


def run(repo, args, data=None, env=None):
    result = subprocess.run([GIT, "-C", str(repo), *args], input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV | (env or {}), timeout=120)
    assert result.returncode == 0, (args, result.returncode, result.stderr.decode())
    return result.stdout


def fixture(n):
    remote = ROOT / f"remote-{n}"
    remote.mkdir()
    run(remote, ["init", "--bare", "--initial-branch=main"])
    run(remote, ["config", "uploadpack.allowFilter", "true"])
    parts = []
    for i in range(n):
        data = hashlib.sha256(str(i).encode()).digest() * 32
        parts.append(f"blob\nmark :{2 * i + 1}\ndata {len(data)}\n".encode() + data + f"\ncommit refs/cc-notes/task/{i}\nmark :{2 * i + 2}\ncommitter Probe <probe@example.invalid> 1700000000 +0000\ndata {len(str(i))}\n{i}\nM 100644 :{2 * i + 1} file\n\n".encode())
    missing_data = b"promised" * 1024
    parts.append(f"blob\nmark :{2 * n + 1}\ndata {len(missing_data)}\n".encode() + missing_data + f"\ncommit refs/heads/main\nmark :{2 * n + 2}\ncommitter Probe <probe@example.invalid> 1700000000 +0000\ndata 4\nmain\nM 100644 :{2 * n + 1} file\n\n".encode())
    run(remote, ["-c", "fastimport.unpackLimit=0", "fast-import", "--quiet"], b"".join(parts))
    client = ROOT / f"client-{n}"
    subprocess.run([GIT, "clone", "--quiet", "--mirror", "--filter=blob:limit=2048", remote.as_uri(), str(client)], env=ENV, check=True, timeout=120)
    packs = list((client / "objects/pack").glob("*.pack"))
    assert len(packs) == 1
    pack = packs[0]
    assert pack.with_suffix(".promisor").is_file()
    listing = run(client, ["verify-pack", "-v", str(pack.with_suffix(".idx"))]).decode()
    (OUT / f"pack-{n}.verify").write_text(listing)
    by_offset = {}
    by_oid = {}
    for line in listing.splitlines():
        fields = line.split()
        if len(fields) >= 5 and re.fullmatch(r"[0-9a-f]{40}", fields[0]):
            row = {"oid": fields[0], "type": fields[1], "inflated_bytes": int(fields[2]), "compressed_bytes": int(fields[3]), "offset": int(fields[4])}
            by_offset[row["offset"]] = row
            by_oid[row["oid"]] = row
    types = collections.Counter(row["type"] for row in by_oid.values())
    assert types == {"blob": n, "commit": n + 1, "tree": n + 1}, types
    base = run(client, ["rev-parse", "refs/heads/main"]).strip().decode()
    base_tree = run(client, ["rev-parse", f"{base}^{{tree}}"]).strip().decode()
    promised = run(client, ["rev-parse", f"{base}:file"]).strip().decode()
    assert promised not in by_oid
    absent = subprocess.run([GIT, "-C", str(client), "cat-file", "-e", promised], stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV, timeout=30)
    assert absent.returncode != 0
    local_blob = run(client, ["hash-object", "-w", "--stdin"], b"local added payload").strip().decode()
    unpromised = run(client, ["hash-object", "-w", "--stdin"], b"unique unpromised absent payload").strip().decode()
    (client / "objects" / unpromised[:2] / unpromised[2:]).unlink()
    return client, pack, base, base_tree, promised, unpromised, local_blob, by_offset


def measure(n, kind, client, pack, base, tree, promised, unpromised, local_blob, by_offset):
    if kind != "packed-tree-boundary":
        selected = promised if kind == "missing-promised" else unpromised
        tree = run(client, ["mktree", "--missing"], f"100644 blob {selected}\tfile\n100644 blob {local_blob}\tnew\n".encode()).strip().decode()
    tip = run(client, ["commit-tree", tree, "-p", base], f"{kind} fixture\n".encode()).strip().decode()
    label = f"{n}-{kind}"
    access = OUT / f"{label}.pack-access"
    trace = OUT / f"{label}.strace"
    args = ["strace", "-f", "-yy", "-s", "512", "-e", "trace=openat,read,mmap,close", "-o", str(trace), GIT, "-C", str(client), "-c", "fetch.hideRefs=refs/cc-notes", "rev-list", "--objects", "--stdin", "--exclude-promisor-objects", "--not", "--exclude-hidden=fetch", "--all", "--quiet", "--alternate-refs"]
    result = subprocess.run(args, input=(tip + "\n").encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV | {"GIT_TRACE_PACK_ACCESS": str(access)}, timeout=120)
    (OUT / f"{label}.stderr").write_bytes(result.stderr)
    seen = set()
    calls = collections.Counter()
    lines = access.read_text().splitlines()
    for line in lines:
        match = re.search(r" (\S+\.pack) ([0-9]+)$", line)
        assert match, line
        logged_pack = Path(match.group(1))
        assert (logged_pack if logged_pack.is_absolute() else client / logged_pack).resolve() == pack.resolve(), line
        offset = int(match.group(2))
        assert offset in by_offset, (offset, line)
        row = by_offset[offset]
        calls[row["type"]] += 1
        seen.add(offset)
    distinct = collections.Counter(by_offset[offset]["type"] for offset in seen)
    pack_read_bytes = 0
    pack_mmap_bytes = 0
    for line in trace.read_text().splitlines():
        assert "<unfinished ...>" not in line and "resumed>" not in line, line
        if str(pack) not in line:
            continue
        if "read(" in line:
            match = re.search(r"\)\s+=\s+([0-9]+)$", line)
            assert match, line
            pack_read_bytes += int(match.group(1))
        if "mmap(" in line:
            match = re.search(r"mmap\([^,]+, ([0-9]+),", line)
            assert match, line
            pack_mmap_bytes += int(match.group(1))
    row = {"count": n, "kind": kind, "exit": result.returncode, "unpack_calls_by_type": dict(calls), "distinct_unpacked_by_type": dict(distinct), "distinct_entry_compressed_bytes": sum(by_offset[offset]["compressed_bytes"] for offset in seen), "pack_read_syscall_bytes": pack_read_bytes, "pack_mmap_mapping_bytes": pack_mmap_bytes, "pack_access_sha256": hashlib.sha256(access.read_bytes()).hexdigest(), "strace_sha256": hashlib.sha256(trace.read_bytes()).hexdigest()}
    REPORT["rows"].append(row)
    (OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")
    assert result.returncode != 0 if kind == "missing-unpromised" else result.returncode == 0, row
    if kind == "missing-unpromised":
        assert unpromised.encode() in result.stderr, row
    assert distinct["blob"] == 0, row
    if kind == "packed-tree-boundary":
        assert 1 <= distinct["commit"] <= 2 and distinct["tree"] <= 1, row
    else:
        assert distinct["commit"] >= n and distinct["tree"] >= n, row


for n in [32, 4096]:
    args = fixture(n)
    for kind in ["packed-tree-boundary", "missing-promised", "missing-unpromised"]:
        measure(n, kind, *args)
print(json.dumps(REPORT, indent=2))
