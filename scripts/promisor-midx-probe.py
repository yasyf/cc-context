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
ROOT = Path(tempfile.mkdtemp(prefix="promisor-midx-"))
GIT = shutil.which("git")
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_NO_LAZY_FETCH="1", GIT_TERMINAL_PROMPT="0", GIT_AUTHOR_NAME="Probe", GIT_AUTHOR_EMAIL="probe@example.invalid", GIT_COMMITTER_NAME="Probe", GIT_COMMITTER_EMAIL="probe@example.invalid", GIT_AUTHOR_DATE="1700000000 +0000", GIT_COMMITTER_DATE="1700000000 +0000")
REPORT = {"git": subprocess.check_output([GIT, "version"], env=ENV, text=True).strip(), "rows": [], "limits": ["StockGit diagnostic only; no .promisor relabeling or production changes", "Fetch child counts establish algorithm path, not physical savings", "MIDX is removed only in private synthetic fixture when changing preferred-copy policy; pack/idx/marker bytes stay unchanged"]}
assert REPORT["git"] == "git version 2.56.0"


def run(repo, args, data=None, env=None):
    result = subprocess.run([GIT, "-C", str(repo), *args], input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV | (env or {}), timeout=120)
    assert result.returncode == 0, (args, result.returncode, result.stderr.decode())
    return result.stdout


def traced_fetch(repo, label, core_midx=True, expected=None):
    trace = OUT / f"{label}.trace2.jsonl"
    args = ["-c", f"core.multiPackIndex={'true' if core_midx else 'false'}", "-c", "maintenance.auto=false", "fetch", "--no-auto-maintenance", "--no-tags", "--no-write-fetch-head", "origin", "refs/heads/main:refs/remotes/origin/main"]
    run(repo, args, env={"GIT_TRACE2_EVENT": str(trace)})
    events = [json.loads(line) for line in trace.read_text().splitlines()]
    children = [event["argv"] for event in events if event.get("event") == "child_start" and "rev-list" in event.get("argv", []) and "--exclude-hidden=fetch" in event["argv"]]
    row = {"label": label, "core_multiPackIndex": core_midx, "exit": 0, "connectivity_children": children, "tracking_oid": run(repo, ["rev-parse", "refs/remotes/origin/main"]).strip().decode(), "trace_sha256": hashlib.sha256(trace.read_bytes()).hexdigest()}
    REPORT["rows"].append(row)
    (OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")
    if expected is not None:
        assert len(children) == expected, row
    return row


def selected_copy(repo, oid, label, expected):
    trace = OUT / f"{label}.pack-access"
    run(repo, ["cat-file", "-p", oid], env={"GIT_TRACE_PACK_ACCESS": str(trace)})
    paths = []
    for line in trace.read_text().splitlines():
        match = re.search(r" (\S+\.pack) ([0-9]+)$", line)
        assert match, line
        paths.append(Path(match.group(1)).name)
    assert expected in paths, (label, expected, paths)
    return paths


for fmt in ["sha1", "sha256"]:
    for n in [32, 4096]:
        label = f"{fmt}-{n}"
        remote = ROOT / f"remote-{label}"
        remote.mkdir()
        run(remote, ["init", "--bare", "--initial-branch=main", f"--object-format={fmt}"])
        run(remote, ["config", "uploadpack.allowFilter", "true"])
        parts = []
        for i in range(n):
            data = str(i)
            text = f"commit refs/heads/main\nmark :{i + 1}\ncommitter Probe <probe@example.invalid> 1700000000 +0000\ndata {len(data)}\n{data}\n"
            if i:
                text += f"from :{i}\n"
            parts.append(text + "\n")
        run(remote, ["-c", "fastimport.unpackLimit=0", "fast-import", "--quiet"], "".join(parts).encode())
        client = ROOT / f"client-{label}"
        clone_trace = OUT / f"{label}-clone.trace2.jsonl"
        subprocess.run([GIT, "clone", "--quiet", "--no-checkout", "--filter=blob:none", remote.as_uri(), str(client)], env=ENV | {"GIT_TRACE2_EVENT": str(clone_trace)}, check=True, timeout=120)
        packs = list((client / ".git/objects/pack").glob("*.pack"))
        layout = {"pack_files": sorted(path.name for path in (client / ".git/objects/pack").iterdir()), "count_objects": run(client, ["count-objects", "-v"]).decode(), "object_format": run(client, ["rev-parse", "--show-object-format"]).decode().strip()}
        (OUT / f"{label}-clone-layout.json").write_text(json.dumps(layout, indent=2) + "\n")
        assert len(packs) == 1, layout
        promisor = packs[0]
        assert promisor.with_suffix(".promisor").is_file()
        base = run(client, ["rev-parse", "refs/remotes/origin/main"]).strip().decode()
        tree = run(client, ["rev-parse", f"{base}^{{tree}}"]).strip().decode()
        p_verify = run(client, ["verify-pack", "-v", str(promisor.with_suffix(".idx"))])
        (OUT / f"{label}-promisor.verify").write_bytes(p_verify)
        traced_fetch(client, f"{label}-only-promisor", expected=0)
        objects = run(client, ["rev-list", "--objects", "--no-object-names", "refs/remotes/origin/main"]).splitlines()
        assert len(objects) == n + 1 and base.encode() in objects
        extra = run(client, ["hash-object", "-w", "--stdin"], b"nonpromisor duplicate pack extra").strip()
        pack_hash = run(client, ["pack-objects", str(client / ".git/objects/pack/pack")], b"\n".join(objects + [extra]) + b"\n").strip().decode()
        nonpromisor = client / ".git/objects/pack" / f"pack-{pack_hash}.pack"
        assert nonpromisor != promisor and nonpromisor.is_file()
        assert not nonpromisor.with_suffix(".promisor").exists()
        n_verify = run(client, ["verify-pack", "-v", str(nonpromisor.with_suffix(".idx"))])
        (OUT / f"{label}-nonpromisor.verify").write_bytes(n_verify)
        identities = {str(path.name): hashlib.sha256(path.read_bytes()).hexdigest() for path in [promisor, promisor.with_suffix(".idx"), promisor.with_suffix(".promisor"), nonpromisor, nonpromisor.with_suffix(".idx")]}
        run(client, ["multi-pack-index", "write", f"--preferred-pack={nonpromisor.name}"])
        run(client, ["multi-pack-index", "verify"])
        n_selected = selected_copy(client, base, f"{label}-preferred-nonpromisor", nonpromisor.name)
        n_row = traced_fetch(client, f"{label}-preferred-nonpromisor")
        traced_fetch(client, f"{label}-same-files-midx-disabled", core_midx=False, expected=0)
        midx = client / ".git/objects/pack/multi-pack-index"
        assert midx.is_file()
        midx.unlink()
        run(client, ["multi-pack-index", "write", f"--preferred-pack={promisor.name}"])
        run(client, ["multi-pack-index", "verify"])
        p_selected = selected_copy(client, base, f"{label}-preferred-promisor", promisor.name)
        p_row = traced_fetch(client, f"{label}-preferred-promisor", expected=0)
        for name, digest in identities.items():
            assert hashlib.sha256((client / ".git/objects/pack" / name).read_bytes()).hexdigest() == digest
        commit = f"tree {tree}\nparent {base}\nauthor Probe <probe@example.invalid> 1700000001 +0000\ncommitter Probe <probe@example.invalid> 1700000001 +0000\n\nlocal nonpromisor tip\n".encode()
        local = run(client, ["hash-object", "-t", "commit", "-w", "--stdin"], commit).strip().decode()
        assert run(remote, ["hash-object", "-t", "commit", "-w", "--stdin"], commit).strip().decode() == local
        run(remote, ["update-ref", "refs/heads/main", local])
        traced_fetch(client, f"{label}-truly-nonpromisor-tip", expected=1)
        REPORT.setdefault("fixtures", []).append({"format": fmt, "commits": n, "wanted": base, "promisor_pack": promisor.name, "nonpromisor_pack": nonpromisor.name, "immutable_pack_idx_marker_sha256": identities, "native_selected_nonpromisor": n_selected, "native_selected_promisor": p_selected, "nonpromisor_preference_connectivity_children": len(n_row["connectivity_children"]), "promisor_preference_connectivity_children": len(p_row["connectivity_children"])})
        (OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")
print(json.dumps(REPORT, indent=2))
