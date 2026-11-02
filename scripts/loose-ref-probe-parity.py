import hashlib
import json
from pathlib import Path
import sys


before = Path(sys.argv[1])
after = Path(sys.argv[2])
output = Path(sys.argv[3])
report = {"fetch": [], "cache": [], "limits": ["CI native semantic and logical syscall controls only", "No physical Mac IO, production fetch attribution or installed-operation claim"]}
for layout in ["loose", "packed"]:
    left_path = before / layout / "report.json"
    right_path = after / layout / "report.json"
    left = json.loads(left_path.read_text())
    right = json.loads(right_path.read_text())
    assert left["ref_layout"] == right["ref_layout"] == layout
    old_rows = {row["label"]: row for row in left["rows"]}
    new_rows = {row["label"]: row for row in right["rows"]}
    assert len(old_rows) == len(left["rows"]) == len(new_rows) == len(right["rows"])
    assert old_rows.keys() == new_rows.keys()
    assert len(old_rows) == 10
    assert {(row["count"], row["hidden"]) for row in old_rows.values() if not row["missing"]} == {(n, hidden) for n in [0, 32, 4096] for hidden in [False, True]}
    assert {(row["count"], row["hidden"]) for row in old_rows.values() if row["missing"]} == {(n, hidden) for n in [0, 32] for hidden in [False, True]}
    for label, old in old_rows.items():
        new = new_rows[label]
        for key in ["count", "hidden", "missing", "exit", "ledger_objects_opened", "all_loose_objects_opened"]:
            assert old[key] == new[key], (layout, label, key, old, new)
        if layout == "loose":
            assert old["hidden_numeric_ref_opens"] == old["count"], (label, old)
            expected = 0 if new["hidden"] else new["count"]
            assert new["hidden_numeric_ref_opens"] == expected, (label, new)
        else:
            assert old["hidden_numeric_ref_opens"] == new["hidden_numeric_ref_opens"] == 0, (label, old, new)
        report["fetch"].append({"layout": layout, "label": label, "count": old["count"], "hidden": old["hidden"], "missing": old["missing"], "exit": old["exit"], "before_ref_opens": old["hidden_numeric_ref_opens"], "after_ref_opens": new["hidden_numeric_ref_opens"]})
    fields = ["label", "hidden", "exit"]
    assert [{key: row[key] for key in fields} for row in left["fetches"]] == [{key: row[key] for key in fields} for row in right["fetches"]]
    report[f"{layout}_report_hashes"] = [hashlib.sha256(left_path.read_bytes()).hexdigest(), hashlib.sha256(right_path.read_bytes()).hexdigest()]

left = json.loads((before / "cache/report.json").read_text())
right = json.loads((after / "cache/report.json").read_text())
assert [row["label"] for row in left["rows"]] == [row["label"] for row in right["rows"]]
assert [row["label"] for row in left["rows"]] == [f"{fmt}-{layout}" for fmt in ["sha1", "sha256"] for layout in ["loose", "packed"]]
for old, new in zip(left["rows"], right["rows"], strict=True):
    label = old["label"]
    for key in ["exit", "direct", "all_before", "all_after", "visible"]:
        assert old[key] == new[key], (label, key, old, new)
    left_raw = (before / "cache" / f"{label}.stdout").read_bytes()
    right_raw = (after / "cache" / f"{label}.stdout").read_bytes()
    assert hashlib.sha256(left_raw).hexdigest() == old["stdout_sha256"]
    assert hashlib.sha256(right_raw).hexdigest() == new["stdout_sha256"]
    assert left_raw.split(b"ALL_BEFORE\n", 1)[1] == right_raw.split(b"ALL_BEFORE\n", 1)[1], label
    assert not any(name.startswith("refs/hidden/") for name in new["filtered_names"]), (label, new)
    report["cache"].append({"label": label, "before_filtered": old["filtered_names"], "after_filtered": new["filtered_names"], "full_before_after_oid_flags_and_order_equal": True, "explicit_lookup_equal": True})

output.write_text(json.dumps(report, indent=2) + "\n")
