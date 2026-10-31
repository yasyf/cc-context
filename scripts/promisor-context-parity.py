import json
from pathlib import Path
import sys

reports = []
for name in sys.argv[1:3]:
    path = Path(name)
    assert path.stat().st_size <= 1_000_000
    reports.append(json.loads(path.read_text()))
assert len(reports) == 2
labels = [
    "ordinary-promised", "ordinary-unpromised", "replace-removes-promised-edge",
    "replace-disabled-control", "replace-adds-unpromised-edge", "replace-added-edge-disabled-control",
    "ordinary-missing-parent", "graft-adds-parent-promise", "shallow-removes-parent-promise",
    "same-pack-without-shallow-parent-rule",
]
fields = ("label", "wanted", "tree", "parent", "target", "exit", "stdout", "stderr", "no_replace", "expected_control")
for report in reports:
    assert report["git"] == "git version 2.56.0"
    assert [row["label"] for row in report["rows"]] == labels
before, after = [[{field: row[field] for field in fields} for row in report["rows"]] for report in reports]
assert before == after, (before, after)
proof = {"status": "STOCK_AND_FIXED_CONTEXT_SEMANTICS_IDENTICAL", "cases": len(labels), "fields": fields, "stock_executable": reports[0]["git_executable"], "stock_sha256": reports[0]["git_sha256"], "fixed_executable": reports[1]["git_executable"], "fixed_sha256": reports[1]["git_sha256"], "rows": after}
with Path(sys.argv[3]).open("x") as output:
    json.dump(proof, output, indent=2)
    output.write("\n")
print(json.dumps({"status": proof["status"], "cases": proof["cases"]}))
