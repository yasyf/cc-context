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
ROOT = Path(tempfile.mkdtemp(prefix="fetch-connectivity-"))
GIT = shutil.which("git")
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_TERMINAL_PROMPT="0", GIT_NO_LAZY_FETCH="1", GIT_AUTHOR_NAME="Probe", GIT_AUTHOR_EMAIL="probe@example.invalid", GIT_COMMITTER_NAME="Probe", GIT_COMMITTER_EMAIL="probe@example.invalid", GIT_AUTHOR_DATE="1700000000 +0000", GIT_COMMITTER_DATE="1700000000 +0000")
HIDE = ["-c", "fetch.hideRefs=refs/cc-notes", "-c", "fetch.hideRefs=refs/cc-notes-sync", "-c", "fetch.hideRefs=refs/ccx"]
REPORT = {"git": subprocess.check_output([GIT, "version"], env=ENV, text=True).strip(), "rows": [], "fetches": [], "limits": ["Synthetic Linux logical reads, not physical Mac throughput", "fetch.hideRefs cannot remove global promisor object enumeration", "No production configuration changed"]}
assert REPORT["git"] == "git version 2.56.0"


def run(repo, args, data=None, env=None, ok=True):
    result = subprocess.run([GIT, "-C", str(repo), *args], input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV | (env or {}), timeout=90)
    if ok:
        assert result.returncode == 0, (args, result.returncode, result.stderr.decode())
    return result


def init(path, bare=False):
    path.mkdir()
    run(path, ["init", *( ["--bare"] if bare else []), "--initial-branch=main"])
    return path


def ledger_repo(n):
    repo = init(ROOT / f"ledger-{n}")
    parts = []
    for i in range(n + 2):
        ref = "refs/heads/main" if i == n else "refs/heads/check" if i == n + 1 else ["refs/cc-notes/task/", "refs/cc-notes-sync/origin/", "refs/ccx/published/"][i % 3] + str(i)
        text = f"commit {ref}\nmark :{i + 1}\ncommitter Probe <probe@example.invalid> 1700000000 +0000\ndata {len(str(i))}\n{i}\n"
        if i == n + 1:
            text += f"from :{n + 1}\n"
        parts.append(text + "\n")
    marks = ROOT / f"marks-{n}"
    run(repo, ["-c", "fastimport.unpackLimit=0", "fast-import", "--quiet", f"--export-marks={marks}"], "".join(parts).encode())
    mapping = dict(line.split() for line in marks.read_text().splitlines())
    wanted = mapping[f":{n + 2}"]
    ledger = {mapping[f":{i + 1}"] for i in range(n)}
    run(repo, ["update-ref", "-d", "refs/heads/check"])
    pack = next((repo / ".git/objects/pack").glob("*.pack"))
    saved = ROOT / f"saved-{n}.pack"
    pack.rename(saved)
    pack.with_suffix(".idx").unlink()
    run(repo, ["unpack-objects", "-r"], saved.read_bytes())
    run(repo, ["pack-refs", "--all"])
    run(repo, ["config", "remote.origin.promisor", "true"])
    return repo, wanted, ledger


def trace_check(repo, wanted, ledger, n, hidden, missing=False):
    label = f"ledger-{n}-{'hidden' if hidden else 'all'}{'-missing' if missing else ''}"
    trace = OUT / f"{label}.strace"
    args = ["strace", "-f", "-yy", "-s", "512", "-e", "trace=openat,read,close", "-o", str(trace), GIT, "-C", str(repo), *(HIDE if hidden else []), "rev-list", "--objects", "--stdin", "--exclude-promisor-objects", "--not", "--exclude-hidden=fetch", "--all", "--quiet", "--alternate-refs"]
    result = subprocess.run(args, input=(wanted + "\n").encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV, timeout=90)
    (OUT / f"{label}.stderr").write_bytes(result.stderr)
    opened = set()
    read_bytes = 0
    for line in trace.read_text().splitlines():
        if "<unfinished ...>" in line or "resumed>" in line:
            raise AssertionError((label, "split trace"))
        match = re.search(r'openat\(.*?"([^"]+)".*\)\s+=\s+([0-9]+)(?:<.*>)?$', line)
        if match:
            oid = re.search(r"(?:^|/)objects/([0-9a-f]{2})/([0-9a-f]{38})$", match.group(1))
            if oid:
                opened.add(oid.group(1) + oid.group(2))
        if "read(" in line and re.search(r"objects/[0-9a-f]{2}/[0-9a-f]{38}>", line):
            size = re.search(r"\)\s+=\s+([0-9]+)$", line)
            assert size, (label, line)
            read_bytes += int(size.group(1))
    row = {"label": label, "count": n, "hidden": hidden, "missing": missing, "exit": result.returncode, "ledger_objects_opened": len(opened & ledger), "all_loose_objects_opened": len(opened), "loose_read_bytes": read_bytes, "trace_sha256": hashlib.sha256(trace.read_bytes()).hexdigest()}
    REPORT["rows"].append(row)
    assert result.returncode != 0 if missing else result.returncode == 0, row
    if not missing:
        assert wanted in opened, row
        assert len(opened & ledger) == (0 if hidden else n), row
    return row


def fetch(repo, remote, refspec, label, hidden):
    trace = OUT / f"{label}.trace2.jsonl"
    result = run(repo, [*(HIDE if hidden else []), "-c", "maintenance.auto=false", "fetch", "--no-auto-maintenance", "--no-write-fetch-head", str(remote), refspec], env={"GIT_TRACE2_EVENT": str(trace)})
    events = [json.loads(line) for line in trace.read_text().splitlines()]
    fallback = [event["argv"] for event in events if event.get("event") == "child_start" and "rev-list" in event.get("argv", []) and "--exclude-hidden=fetch" in event["argv"]]
    REPORT["fetches"].append({"label": label, "hidden": hidden, "exit": result.returncode, "fallback": fallback, "trace_sha256": hashlib.sha256(trace.read_bytes()).hexdigest()})
    return fallback


for n in [32, 4096]:
    repo, wanted, ledger = ledger_repo(n)
    for hidden in [False, True]:
        trace_check(repo, wanted, ledger, n, hidden)
    if n == 32:
        target = repo / ".git/objects" / wanted[:2] / wanted[2:]
        saved = target.read_bytes()
        target.unlink()
        for hidden in [False, True]:
            trace_check(repo, wanted, ledger, n, hidden, missing=True)
        target.write_bytes(saved)

missing_repo = init(ROOT / "missing-reachable")
missing_blob = run(missing_repo, ["hash-object", "-w", "--stdin"], b"missing reachable payload").stdout.strip().decode()
missing_tree = run(missing_repo, ["mktree"], f"100644 blob {missing_blob}\tfile\n".encode()).stdout.strip().decode()
missing_commit = run(missing_repo, ["commit-tree", missing_tree], b"missing reachable fixture\n").stdout.strip().decode()
run(missing_repo, ["config", "remote.origin.promisor", "true"])
(missing_repo / ".git/objects" / missing_blob[:2] / missing_blob[2:]).unlink()
for hidden in [False, True]:
    trace_check(missing_repo, missing_commit, set(), 0, hidden, missing=True)

for hidden in [False, True]:
    remote = init(ROOT / f"remote-{hidden}", bare=True)
    run(remote, ["config", "uploadpack.allowFilter", "true"])
    run(remote, ["config", "uploadpack.allowAnySHA1InWant", "true"])
    run(remote, ["fast-import", "--quiet"], b"blob\nmark :1\ndata 8\npayload\n\ncommit refs/heads/main\nmark :2\ncommitter Probe <probe@example.invalid> 1700000000 +0000\ndata 4\nbase\nM 100644 :1 file\n\n")
    client = ROOT / f"client-{hidden}"
    subprocess.run([GIT, "clone", "--quiet", "--no-checkout", "--filter=blob:none", remote.as_uri(), str(client)], env=ENV, check=True, timeout=90)
    assert not fetch(client, remote.as_uri(), "refs/heads/main:refs/remotes/origin/main", f"promisor-{hidden}", hidden)
    base = run(client, ["rev-parse", "refs/remotes/origin/main"]).stdout.strip().decode()
    tree = run(client, ["rev-parse", f"{base}^{{tree}}"]).stdout.strip().decode()
    promised_blob = run(client, ["rev-parse", f"{base}:file"]).stdout.strip().decode()
    assert run(client, ["cat-file", "-e", promised_blob], ok=False).returncode != 0
    commit = f"tree {tree}\nparent {base}\nauthor Probe <probe@example.invalid> 1700000001 +0000\ncommitter Probe <probe@example.invalid> 1700000001 +0000\n\nlocal tip\n".encode()
    local = run(client, ["hash-object", "-t", "commit", "-w", "--stdin"], commit).stdout.strip().decode()
    assert run(remote, ["hash-object", "-t", "commit", "-w", "--stdin"], commit).stdout.strip().decode() == local
    run(remote, ["update-ref", "refs/heads/main", local])
    assert base != local
    assert run(client, ["rev-parse", "refs/remotes/origin/main"]).stdout.strip().decode() == base
    assert fetch(client, remote.as_uri(), "refs/heads/main:refs/remotes/origin/main", f"mixed-{hidden}", hidden)
    assert run(client, ["rev-parse", "refs/remotes/origin/main"]).stdout.strip().decode() == local
    metadata = run(remote, ["hash-object", "-w", "--stdin"], b"notes sync payload").stdout.strip().decode()
    run(remote, ["update-ref", "refs/cc-notes/task/example", metadata])
    assert run(client, ["show-ref", "--verify", "--quiet", "refs/cc-notes-sync/origin/task/example"], ok=False).returncode != 0
    assert run(client, ["cat-file", "-e", metadata], ok=False).returncode != 0
    fetch(client, remote.as_uri(), "refs/cc-notes/*:refs/cc-notes-sync/origin/*", f"metadata-{hidden}", hidden)
    assert run(client, ["rev-parse", "refs/cc-notes-sync/origin/task/example"]).stdout.strip().decode() == metadata
    assert run(client, ["cat-file", "blob", "refs/cc-notes-sync/origin/task/example"]).stdout == b"notes sync payload"
    assert run(client, [*(HIDE if hidden else []), "rev-list", "--exclude-promisor-objects", "--all", "--objects"]).returncode == 0
    retained = run(client, ["hash-object", "-w", "--stdin"], b"local metadata retained").stdout.strip().decode()
    orphan = run(client, ["hash-object", "-w", "--stdin"], b"unreferenced deletion control").stdout.strip().decode()
    run(client, ["update-ref", "refs/cc-notes/task/retention", retained])
    retained_path = client / ".git/objects" / retained[:2] / retained[2:]
    orphan_path = client / ".git/objects" / orphan[:2] / orphan[2:]
    for path in [retained_path, orphan_path]:
        assert path.is_file()
        os.utime(path, (1700000000, 1700000000))
    run(client, [*(HIDE if hidden else []), "prune", "--expire=now"])
    assert retained_path.is_file()
    assert not orphan_path.exists()
    assert run(client, ["cat-file", "blob", "refs/cc-notes/task/retention"]).stdout == b"local metadata retained"
    REPORT.setdefault("retention", []).append({"hidden": hidden, "retained_loose_metadata": True, "unreferenced_loose_control_deleted": True})
(OUT / "report.json").write_text(json.dumps(REPORT, indent=2) + "\n")
print(json.dumps(REPORT, indent=2))
