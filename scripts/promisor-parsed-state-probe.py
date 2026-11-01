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
ROOT = Path(tempfile.mkdtemp(prefix="promisor-native-state-"))
GIT = shutil.which("git")
TOOL = shutil.which("test-tool")
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_TERMINAL_PROMPT="0", GIT_NO_LAZY_FETCH="1", GIT_AUTHOR_NAME="Probe", GIT_AUTHOR_EMAIL="probe@example.invalid", GIT_COMMITTER_NAME="Probe", GIT_COMMITTER_EMAIL="probe@example.invalid", GIT_AUTHOR_DATE="1700000000 +0000", GIT_COMMITTER_DATE="1700000000 +0000")
REPORT = {"git": GIT, "git_sha256": hashlib.sha256(Path(GIT).read_bytes()).hexdigest(), "tool": TOOL, "tool_sha256": hashlib.sha256(Path(TOOL).read_bytes()).hexdigest(), "rows": [], "graft_rows": [], "limits": ["CI-only native API diagnostic; no production Git source change", "One is_promisor_object per fresh helper process", "Raw parse uses native physical-object read and parse_object_buffer; not a claim ordinary fetch mutates its in-memory parse context", "Configured commit graph is credited only by actual native graph position", "No derived index or promise-cache trust/lifetime approval", "No physical host IO or historical fetch/status attribution"]}
assert subprocess.check_output([GIT, "version"], env=ENV, text=True).strip() == "git version 2.56.0"


def save():
    (OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")


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


def missing(repo, kind, payload):
    oid = run(repo, ["hash-object", "-t", kind, "-w", "--stdin"], payload).strip().decode()
    (repo / "objects" / oid[:2] / oid[2:]).unlink()
    return oid


def tag(repo, target, label):
    data = f"object {target}\ntype blob\ntag {label}\ntagger Probe <probe@example.invalid> 1700000000 +0000\n\n{label}\n"
    return run(repo, ["hash-object", "-t", "tag", "-w", "--stdin"], data.encode()).strip().decode()


def helper(repo, command, arguments, graph, label, no_replace=False):
    env = ENV | {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.commitGraph", "GIT_CONFIG_VALUE_0": str(graph).lower()}
    if no_replace:
        env["GIT_NO_REPLACE_OBJECTS"] = "1"
    result = subprocess.run([TOOL, "-C", str(repo), command, *arguments], stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, timeout=120)
    row = {"label": label, "command": command, "arguments": arguments, "graph_configured": graph, "no_replace": no_replace, "exit": result.returncode, "stdout": result.stdout.decode(), "stderr": result.stderr.decode()}
    REPORT["graft_rows" if command == "promisor-parsed-state" else "rows"].append(row)
    save()
    assert result.returncode == 0, row
    row["native"] = json.loads(row["stdout"])
    save()
    return row["native"]


def check_object(repo, kind, source, target, marker, label, expected, raw_expected=None, no_replace=False):
    raw_expected = expected if raw_expected is None else raw_expected
    for graph in (False, True):
        values = {}
        modes = ("fresh", "parsed", "raw") + (("reset",) if kind != "tag" else ())
        for mode in modes:
            value = helper(repo, "promisor-object-state", [mode, source, target, marker], graph, label, no_replace)
            assert value["parsed_before"] == int(mode in ("parsed", "raw")), (label, mode, value)
            assert value["source_type"] == {"commit": 1, "tree": 2, "tag": 4}[kind], value
            assert value["promised"] == (raw_expected if mode == "raw" else expected), (label, mode, value)
            if not graph or (label.startswith("replace-") and not no_replace):
                assert value["graph_position"] == 0xFFFFFFFF, (label, graph, mode, value)
            if graph and kind == "commit" and not label.startswith("replace-") and mode != "raw":
                assert value["graph_position"] != 0xFFFFFFFF, (label, graph, mode, value)
            values[mode] = value["promised"]
        assert values["fresh"] == values["parsed"], (label, values)
        if "reset" in values:
            assert values["reset"] == values["fresh"], (label, values)


client, pack, base, base_tree, promised, unpromised, local_blob, by_offset = fixture(32)
run(client, ["-c", "core.commitGraph=true", "commit-graph", "write", "--reachable"])
run(client, ["commit-graph", "verify"])
graph_file = client / "objects/info/commit-graph"
assert graph_file.is_file()
REPORT["commit_graph"] = {"sha256": hashlib.sha256(graph_file.read_bytes()).hexdigest(), "bytes": graph_file.stat().st_size}
tag_a = missing(client, "blob", b"tag promised missing payload")
tag_b = missing(client, "blob", b"tag replacement missing payload")
source_tag = tag(client, tag_a, "physical-promisor-tag")
replacement_tag = tag(client, tag_b, "replacement-tag")
data = run(client, ["pack-objects", "--stdout"], (source_tag + "\n").encode())
run(client, ["index-pack", "--stdin", "--promisor"], data)
(client / "objects" / source_tag[:2] / source_tag[2:]).unlink()
run(client, ["cat-file", "-e", source_tag])
parent_body = f"tree {base_tree}\nparent {base}\nauthor Probe <probe@example.invalid> 1700000001 +0000\ncommitter Probe <probe@example.invalid> 1700000001 +0000\n\nmissing parent\n".encode()
missing_parent = missing(client, "commit", parent_body)
for target in (promised, unpromised, tag_a, tag_b, missing_parent):
    result = subprocess.run([GIT, "-C", str(client), "cat-file", "-e", target], env=ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
    assert result.returncode != 0, target
packs_before = {f.name: hashlib.sha256(f.read_bytes()).hexdigest() for f in (client / "objects/pack").iterdir() if f.suffix in (".pack", ".idx", ".promisor")}
assert len([name for name in packs_before if name.endswith(".promisor")]) == 2
REPORT["fixture"] = {"base": base, "tree": base_tree, "tag": source_tag, "promised": promised, "unpromised": unpromised, "tag_a": tag_a, "tag_b": tag_b, "missing_parent": missing_parent, "packs": packs_before}
save()
for kind, source, target in (("commit", base, promised), ("tree", base_tree, promised), ("tag", source_tag, tag_a)):
    check_object(client, kind, source, target, base, "ordinary-" + kind + "-promised", 1)
    check_object(client, kind, source, unpromised, base, "ordinary-" + kind + "-unpromised", 0)
for graph in (False, True):
    for mode in ("fresh", "parsed", "unparsed"):
        value = helper(client, "promisor-parsed-state", [mode, base, missing_parent], graph, "existing-graft-replacement")
        assert value["parsed_before"] == int(mode == "parsed"), (mode, value)
        assert value["parents_before"] == 0, (mode, value)
        assert value["promised"] == int(mode != "parsed"), (mode, value)
        assert value["parents_after"] == int(mode != "parsed"), (mode, value)
empty = run(client, ["mktree"], b"").strip().decode()
added_tree = run(client, ["mktree", "--missing"], f"100644 blob {unpromised}\tfile\n".encode()).strip().decode()
replacement_commit_body = f"tree {base_tree}\nparent {missing_parent}\nauthor Probe <probe@example.invalid> 1700000002 +0000\ncommitter Probe <probe@example.invalid> 1700000002 +0000\n\nreplacement parent\n".encode()
replacement_commit = run(client, ["hash-object", "-t", "commit", "-w", "--stdin"], replacement_commit_body).strip().decode()
for kind, source, replacement, targets in [
    ("tree", base_tree, empty, [(promised, 0, 1, "removes")]),
    ("tree", base_tree, added_tree, [(unpromised, 1, 0, "adds")]),
    ("tag", source_tag, replacement_tag, [(tag_a, 0, 1, "removes"), (tag_b, 1, 0, "adds")]),
    ("commit", base, replacement_commit, [(missing_parent, 1, 0, "adds")]),
]:
    run(client, ["replace", source, replacement])
    for target, expected, raw_expected, direction in targets:
        label = f"replace-{kind}-{direction}"
        check_object(client, kind, source, target, base, label, expected, raw_expected)
        check_object(client, kind, source, target, base, label + "-disabled", raw_expected, no_replace=True)
    run(client, ["replace", "-d", source])
assert packs_before == {f.name: hashlib.sha256(f.read_bytes()).hexdigest() for f in (client / "objects/pack").iterdir() if f.suffix in (".pack", ".idx", ".promisor")}
REPORT["immutable_packs_unchanged"] = True
REPORT["status"] = "NATIVE_API_CONTROLS_PASS_NOT_INDEX_APPROVAL"
save()
print(json.dumps({"status": REPORT["status"], "object_processes": len(REPORT["rows"]), "graft_processes": len(REPORT["graft_rows"])}))
shutil.rmtree(ROOT)
