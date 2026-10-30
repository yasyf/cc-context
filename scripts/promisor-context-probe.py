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
ROOT = Path(tempfile.mkdtemp(prefix="promisor-context-"))
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


def tip(repo, tree, parent, label):
    commit = f"tree {tree}\nparent {parent}\nauthor Probe <probe@example.invalid> 1700000001 +0000\ncommitter Probe <probe@example.invalid> 1700000001 +0000\n\n{label}\n".encode()
    return run(repo, ["hash-object", "-t", "commit", "-w", "--stdin"], commit).strip().decode()


def query(repo, tree, parent, label, target, no_replace=False, expected=None):
    wanted = tip(repo, tree, parent, label)
    args = [GIT, "-C", str(repo), *(["--no-replace-objects"] if no_replace else []), "-c", "fetch.hideRefs=refs/cc-notes", "-c", "fetch.hideRefs=refs/replace", "-c", "fetch.hideRefs=refs/heads/main", "-c", "fetch.hideRefs=HEAD", "rev-list", "--objects", "--stdin", "--exclude-promisor-objects", "--not", "--exclude-hidden=fetch", "--all", "--quiet", "--alternate-refs"]
    result = subprocess.run(args, input=(wanted + "\n").encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV, timeout=120)
    row = {"label": label, "argv": args, "wanted": wanted, "tree": tree, "parent": parent, "target": target, "exit": result.returncode, "stdout": result.stdout.decode(), "stderr": result.stderr.decode(), "no_replace": no_replace, "expected_control": expected}
    REPORT["rows"].append(row)
    (OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")
    if expected is not None:
        assert result.returncode == expected, row
        if expected:
            assert target in row["stderr"], row
    return row


client, pack, base, base_tree, promised, unpromised, local_blob, by_offset = fixture(32)
original_pack_sha = hashlib.sha256(pack.read_bytes()).hexdigest()
new_tree = run(client, ["mktree", "--missing"], f"100644 blob {promised}\tfile\n100644 blob {local_blob}\tnew\n".encode()).strip().decode()
unpromised_tree = run(client, ["mktree", "--missing"], f"100644 blob {unpromised}\tfile\n100644 blob {local_blob}\tnew\n".encode()).strip().decode()
query(client, new_tree, base, "ordinary-promised", promised, expected=0)
query(client, unpromised_tree, base, "ordinary-unpromised", unpromised, expected=128)
empty = run(client, ["mktree"], b"").strip().decode()
run(client, ["replace", base_tree, empty])
query(client, new_tree, base, "replace-removes-promised-edge", promised)
query(client, new_tree, base, "replace-disabled-control", promised, no_replace=True, expected=0)
run(client, ["replace", "-d", base_tree])
run(client, ["replace", base_tree, unpromised_tree])
query(client, unpromised_tree, base, "replace-adds-unpromised-edge", unpromised)
query(client, unpromised_tree, base, "replace-added-edge-disabled-control", unpromised, no_replace=True, expected=128)
run(client, ["replace", "-d", base_tree])
missing_parent = tip(client, base_tree, base, "graft missing parent source")
(client / "objects" / missing_parent[:2] / missing_parent[2:]).unlink()
query(client, base_tree, missing_parent, "ordinary-missing-parent", missing_parent, expected=128)
grafts = client / "info/grafts"
grafts.parent.mkdir(exist_ok=True)
grafts.write_text(f"{base} {missing_parent}\n")
query(client, base_tree, missing_parent, "graft-adds-parent-promise", missing_parent)
grafts.unlink()
assert hashlib.sha256(pack.read_bytes()).hexdigest() == original_pack_sha
REPORT["immutable_pack_same_across_contexts"] = {"pack": pack.name, "sha256": original_pack_sha}
remote = ROOT / "remote-32"
remote_child = run(remote, ["commit-tree", base_tree, "-p", base], b"new shallow tip\n").strip().decode()
run(remote, ["update-ref", "refs/heads/main", remote_child])
shallow = ROOT / "shallow-client"
shallow.mkdir()
run(shallow, ["init", "--bare", "--initial-branch=main"])
run(shallow, ["remote", "add", "origin", remote.as_uri()])
run(shallow, ["config", "remote.origin.promisor", "true"])
run(shallow, ["config", "remote.origin.partialclonefilter", "blob:limit=2048"])
run(shallow, ["fetch", "--no-auto-maintenance", "--depth=1", "--filter=blob:limit=2048", "origin", "refs/heads/main:refs/heads/main"])
absent = subprocess.run([GIT, "-C", str(shallow), "cat-file", "-e", base], stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV, timeout=30)
assert absent.returncode != 0
shallow_file = shallow / "shallow"
assert remote_child in shallow_file.read_text()
query(shallow, base_tree, base, "shallow-removes-parent-promise", base)
shallow_file.unlink()
query(shallow, base_tree, base, "same-pack-without-shallow-parent-rule", base)
REPORT["limits"] = ["StockGit semantic data collection only; mutated-context outcomes are observations, not index-design approval", "Same native immutable source pack can be interpreted under different replace/graft/shallow contexts", "Diagnostic hides refs/replace, main and HEAD from negative roots to isolate promise classification; this is NOT production hideRefs policy", "No cache/index prototype, production config or user data", "Already-parsed-state API harness and commit-graph matrix remain separate gates"]
(OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")
print(json.dumps(REPORT, indent=2))
