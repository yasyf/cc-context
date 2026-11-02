import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


git = Path(os.environ["PROBE_GIT"]).resolve()
helper = Path(os.environ["PROBE_TEST_TOOL"]).resolve()
output = Path(os.environ["PROBE_OUTPUT"])
output.mkdir(parents=True, exist_ok=True)
rows = []


with tempfile.TemporaryDirectory(prefix="loose-ref-cache-") as owned:
    root = Path(owned)
    env = {
        "PATH": os.environ["PATH"],
        "HOME": str(root),
        "LC_ALL": "C",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_NO_LAZY_FETCH": "1",
        "GIT_AUTHOR_NAME": "Probe",
        "GIT_AUTHOR_EMAIL": "probe@example.invalid",
        "GIT_COMMITTER_NAME": "Probe",
        "GIT_COMMITTER_EMAIL": "probe@example.invalid",
        "GIT_AUTHOR_DATE": "1700000000 +0000",
        "GIT_COMMITTER_DATE": "1700000000 +0000",
    }

    def run(repo, args, data=None):
        result = subprocess.run(
            [str(git), "-C", str(repo), *args],
            input=data, capture_output=True, env=env, check=True, timeout=30,
        )
        return result.stdout.decode().strip()

    for fmt in ["sha1", "sha256"]:
        for layout in ["loose", "packed"]:
            repo = root / f"{fmt}-{layout}"
            repo.mkdir()
            run(repo, ["init", f"--object-format={fmt}", "--initial-branch=main"])
            tree = run(repo, ["mktree"], b"")
            old = run(repo, ["commit-tree", tree], b"before\n")
            new = run(repo, ["commit-tree", tree], b"after\n")
            names = ["refs/heads/main", "refs/hidden/updated", "refs/hidden/deleted", "refs/hidden-sibling/kept"]
            for name in names:
                run(repo, ["update-ref", name, old])
            run(repo, ["symbolic-ref", "refs/heads/alias", "refs/hidden/updated"])
            if layout == "packed":
                run(repo, ["pack-refs", "--all"])
            args = [str(helper), "ref-store", "main", "excluded-cache", "refs/hidden", "refs/hidden/updated", "refs/hidden/deleted", "refs/hidden/created", new]
            result = subprocess.run(args, cwd=repo, capture_output=True, env=env, check=True, timeout=30)
            raw = result.stdout
            label = f"{fmt}-{layout}"
            (output / f"{label}.stdout").write_bytes(raw)
            (output / f"{label}.stderr").write_bytes(result.stderr)
            sections = {"FILTERED": {}, "ALL_BEFORE": {}, "ALL_AFTER": {}}
            phase = None
            direct = None
            for line in raw.decode().splitlines():
                if line in sections:
                    phase = line
                elif line.startswith("DIRECT "):
                    _, name, oid = line.split()
                    direct = [name, oid]
                else:
                    oid, name, flags = line.split()
                    assert phase is not None and name not in sections[phase], (label, phase, line)
                    sections[phase][name] = oid
                    int(flags, 16)
            before = {name: old for name in names + ["refs/heads/alias"]}
            after = before | {"refs/hidden/updated": new, "refs/hidden/created": new, "refs/heads/alias": new}
            del after["refs/hidden/deleted"]
            assert sections["ALL_BEFORE"] == before, (label, sections)
            assert sections["ALL_AFTER"] == after, (label, sections)
            assert direct == ["refs/hidden/updated", old], (label, direct)
            visible = {name: oid for name, oid in before.items() if not name.startswith("refs/hidden/")}
            assert {name: oid for name, oid in sections["FILTERED"].items() if not name.startswith("refs/hidden/")} == visible, (label, sections)
            rows.append({"label": label, "exit": result.returncode, "direct": direct, "all_before": sections["ALL_BEFORE"], "all_after": sections["ALL_AFTER"], "visible": visible, "filtered_names": sorted(sections["FILTERED"]), "stdout_sha256": hashlib.sha256(raw).hexdigest(), "stderr_sha256": hashlib.sha256(result.stderr).hexdigest()})

report = {"rows": rows, "limits": ["Native same-process cache completeness and generic transaction controls only", "Best-effort filtered populations may differ; full before/after populations and explicit resolution are exact", "No physical IO, host recovery or publication/install claim"]}
(output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
