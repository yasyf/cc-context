import json
import pathlib
import re
import subprocess
import sys


def measure(binary, output):
    trace = output.with_suffix(".strace")
    subprocess.run(
        [
            "strace", "-ff", "-yy", "-e",
            "trace=openat,read,close,fstat,newfstatat,statx",
            "-o", str(trace), str(binary),
            "-test.run=^TestRetiredHoldingsMetadataSyscalls$", "-test.v",
        ],
        check=True,
    )
    lines = [
        line
        for thread in trace.parent.glob(trace.name + ".*")
        for line in thread.read_text().splitlines()
        if "ccx-census-syscalls-" in line
    ]
    stats = [line for line in lines if re.search(r"\b(?:fstat|newfstatat|statx)\(", line) and "= 0" in line]
    metadata = r"/(?:gitdir|HEAD|\.git)(?:>|\")"
    result = {
        "metadata_stats": sum(bool(re.search(metadata, line)) for line in stats),
        "identity_stats": sum(bool(re.search(r'/admin"', line)) for line in stats),
        "metadata_opens": sum("openat(" in line and "O_RDONLY" in line and bool(re.search(metadata, line)) for line in lines),
        "metadata_read_bytes": sum(int(match.group(1)) for line in lines if re.search(metadata, line) and (match := re.search(r"\bread\(.*\)\s+= (\d+)$", line))),
    }
    output.write_text(json.dumps(result, indent=2) + "\n")
    return result


def main():
    root = pathlib.Path(sys.argv[1])
    baseline = measure(root / "baseline.test", root / "baseline.json")
    fixed = measure(root / "fixed.test", root / "fixed.json")
    assert baseline["metadata_stats"] == 3, baseline
    assert fixed["metadata_stats"] == 0, fixed
    assert baseline["identity_stats"] == fixed["identity_stats"] == 2, (baseline, fixed)
    assert baseline["metadata_opens"] == fixed["metadata_opens"] == 3, (baseline, fixed)
    assert baseline["metadata_read_bytes"] == fixed["metadata_read_bytes"] > 0, (baseline, fixed)
    print(json.dumps({"baseline": baseline, "fixed": fixed}, indent=2))


if __name__ == "__main__":
    main()
