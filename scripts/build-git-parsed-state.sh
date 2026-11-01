#!/usr/bin/env bash
set -euo pipefail

version=2.56.0
sha256=26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3
prefix="${1:?usage: build-git-parsed-state.sh <prefix>}"
work="$(mktemp -d "$RUNNER_TEMP/git-parsed-state.XXXXXX")"
trap 'rm -rf "$work"' EXIT

sudo apt-get update -q
sudo apt-get install -y -q --no-install-recommends libcurl4-openssl-dev zlib1g-dev
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
	-o "$work/git.tar.xz" "https://kernel.org/pub/software/scm/git/git-$version.tar.xz"
echo "$sha256  $work/git.tar.xz" | shasum -a 256 -c -
tar -xJf "$work/git.tar.xz" -C "$work"
source="$work/git-$version"
echo "021b1b9b25551070a3a1f986d1972ef93ef9695c3ff4f60eeb0a9a0798b94032  $source/packfile.c" | shasum -a 256 -c -
echo "bfb57f4c3f4ff9428273106afa03296344b777db5b0de3390514db7d987cec4a  $source/odb/source-packed.c" | shasum -a 256 -c -
python3 - "$source" "$GITHUB_WORKSPACE" <<'PY'
import hashlib
import json
from pathlib import Path
import sys

source, repository = map(Path, sys.argv[1:])
before = {
    str(path.relative_to(source)): hashlib.sha256(path.read_bytes()).hexdigest()
    for path in source.rglob("*") if path.is_file()
}
names = ("promisor-parsed-state", "promisor-object-state")
for name in names:
    (source / "t/helper" / f"test-{name}.c").write_bytes(
        (repository / "scripts" / f"test-{name}.c").read_bytes()
    )
for filename, anchor, additions in [
    ("Makefile", "TEST_BUILTINS_OBJS += test-partial-clone.o\n",
     "".join(f"TEST_BUILTINS_OBJS += test-{name}.o\n" for name in names)),
    ("t/helper/test-tool.h", "int cmd__partial_clone(int argc, const char **argv);\n",
     "".join(f"int cmd__{name.replace('-', '_')}(int argc, const char **argv);\n" for name in names)),
    ("t/helper/test-tool.c", '\t{ "partial-clone", cmd__partial_clone },\n',
     "".join(f'\t{{ "{name}", cmd__{name.replace("-", "_")} }},\n' for name in names)),
]:
    path = source / filename
    text = path.read_text()
    assert text.count(anchor) == 1, filename
    path.write_text(text.replace(anchor, anchor + additions))
after = {
    str(path.relative_to(source)): hashlib.sha256(path.read_bytes()).hexdigest()
    for path in source.rglob("*") if path.is_file()
}
changed = sorted(name for name in before.keys() | after.keys()
                 if before.get(name) != after.get(name))
assert changed == sorted([
    "Makefile", "t/helper/test-tool.h", "t/helper/test-tool.c",
    "t/helper/test-promisor-parsed-state.c", "t/helper/test-promisor-object-state.c",
]), changed
output = repository / "parsed-state-build-source.json"
output.write_text(json.dumps({"changed": changed, "postimages": {name: after[name] for name in changed},
                              "production_source_unchanged": True}, indent=2) + "\n")
PY

flags=(
	prefix="$prefix"
	NO_RUST=YesPlease
	NO_OPENSSL=YesPlease
	NO_GETTEXT=YesPlease
	NO_TCLTK=YesPlease
	NO_PERL=YesPlease
	NO_PYTHON=YesPlease
	NO_EXPAT=YesPlease
	NO_HOMEBREW=YesPlease
	NO_DARWIN_PORTS=YesPlease
	NO_FINK=YesPlease
)
make -C "$source" -j2 "${flags[@]}" all t/helper/test-tool
make -C "$source" "${flags[@]}" install
cp "$source/t/helper/test-tool" "$prefix/bin/test-tool"
"$prefix/bin/git" version
