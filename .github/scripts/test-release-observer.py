import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest

FAKE_GH = r"""import json
import os
import sys

args = sys.argv[1:]
entry = {"args": args}
if args[0] == "api":
    entry["input"] = json.load(sys.stdin)
with open(os.environ["CALL_LOG"], "a") as log:
    log.write(json.dumps(entry) + "\n")
if args[0] == "api":
    path = next(arg for arg in args if arg.startswith("repos/"))
    print(json.dumps({"workflow_run_id": 101 if path.endswith("/ci.yml/dispatches") else 102}))
elif args[:2] == ["run", "watch"]:
    if os.environ["WATCH_EXIT"] != "0":
        print(os.environ["WATCH_ERROR"], file=sys.stderr)
    sys.exit(int(os.environ["WATCH_EXIT"]))
elif args[:2] == ["run", "view"]:
    if os.environ["VIEW_EXIT"] != "0":
        print("HTTP 502: observation unavailable", file=sys.stderr)
    else:
        print(os.environ["OUTCOME"])
    sys.exit(int(os.environ["VIEW_EXIT"]))
else:
    raise AssertionError(args)
"""


def release_script():
    source = Path(".github/workflows/release.yml").read_text()
    step = source.split("      - name: Dispatch and wait for post-release checks\n", 1)[1]
    block = step.split("        run: |\n", 1)[1]
    lines = []
    for line in block.splitlines():
        if line and not line.startswith("          "):
            break
        lines.append(line)
    return textwrap.dedent("\n".join(lines)) + "\n"


class ReleaseObserverTest(unittest.TestCase):
    def test_observer_outcomes(self):
        cases = [
            ("watch_success", 0, "", 0, True, ""),
            ("http502_success", 1, "completed\tsuccess", 0, True, ""),
            ("tls_success", 1, "completed\tsuccess", 0, True, ""),
            ("failure", 1, "completed\tfailure", 0, False, "completed with failure"),
            ("cancelled", 1, "completed\tcancelled", 0, False, "completed with cancelled"),
            ("timed_out", 1, "completed\ttimed_out", 0, False, "completed with timed_out"),
            ("neutral", 1, "completed\tneutral", 0, False, "completed with neutral"),
            ("queued", 1, "queued\t", 0, False, "outcome UNKNOWN"),
            ("in_progress", 1, "in_progress\t", 0, False, "outcome UNKNOWN"),
            ("missing_conclusion", 1, "completed\t", 0, False, "outcome UNKNOWN"),
            ("unavailable", 1, "", 1, False, "outcome UNKNOWN: observation unavailable"),
        ]
        for name, watch_exit, outcome, view_exit, success, diagnostic in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                gh = root / "gh"
                gh.write_text("#!" + sys.executable + "\n" + FAKE_GH)
                gh.chmod(0o700)
                calls = root / "calls.jsonl"
                env = os.environ | {
                    "PATH": str(root) + os.pathsep + os.environ["PATH"],
                    "COMMIT": "f" * 40,
                    "GITHUB_REPOSITORY": "fixture/repo",
                    "GH_TOKEN": "fixture",
                    "CALL_LOG": str(calls),
                    "WATCH_EXIT": str(watch_exit),
                    "WATCH_ERROR": "TLS handshake timeout" if name == "tls_success" else "failed to get jobs: HTTP 502",
                    "OUTCOME": outcome,
                    "VIEW_EXIT": str(view_exit),
                }
                result = subprocess.run(["bash", "-e", "-o", "pipefail"], input=release_script(), env=env, text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode == 0, success, result.stderr)
                if diagnostic:
                    self.assertIn(diagnostic, result.stderr)
                if "UNKNOWN" in diagnostic:
                    self.assertNotIn("completed with", result.stderr)
                records = [json.loads(line) for line in calls.read_text().splitlines()]
                dispatches = [row for row in records if row["args"][0] == "api"]
                watchers = [row for row in records if row["args"][:2] == ["run", "watch"]]
                views = [row for row in records if row["args"][:2] == ["run", "view"]]
                self.assertEqual(len(dispatches), 2)
                self.assertEqual({next(arg for arg in row["args"] if arg.startswith("repos/")) for row in dispatches}, {"repos/fixture/repo/actions/workflows/ci.yml/dispatches", "repos/fixture/repo/actions/workflows/guides.yml/dispatches"})
                for row in dispatches:
                    self.assertEqual(row["args"][row["args"].index("--method") + 1], "POST")
                    self.assertEqual(row["input"], {"ref": "main", "inputs": {"commit": "f" * 40}})
                self.assertEqual(len(watchers), 2)
                self.assertEqual({row["args"][2] for row in watchers}, {"101", "102"})
                self.assertEqual(len(views), 2 if watch_exit else 0)
                if views:
                    self.assertEqual({row["args"][2] for row in views}, {"101", "102"})
                for row in watchers + views:
                    self.assertEqual(row["args"][row["args"].index("--repo") + 1], "fixture/repo")
                print("observer case " + name + " PASS")


if __name__ == "__main__":
    unittest.main()
