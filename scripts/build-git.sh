#!/usr/bin/env bash
set -euo pipefail

version=2.56.0
sha256=26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3

prefix="${1:?usage: build-git.sh <prefix>}"
work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/git-build.XXXXXX")"
trap 'rm -rf "$work"' EXIT

if [ "$(uname -s)" = Linux ]; then
	sudo apt-get update -q
	sudo apt-get install -y -q --no-install-recommends libcurl4-openssl-dev zlib1g-dev
fi

tarball="$work/git-$version.tar.xz"
curl --fail --silent --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
	-o "$tarball" "https://kernel.org/pub/software/scm/git/git-$version.tar.xz"
echo "$sha256  $tarball" | shasum -a 256 -c -
tar -xJf "$tarball" -C "$work"

# NO_HOMEBREW/NO_DARWIN_PORTS/NO_FINK keep macOS from linking Homebrew, MacPorts,
# or Fink libraries a cache-restored binary cannot count on.
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
make -C "$work/git-$version" -j2 "${flags[@]}" all
make -C "$work/git-$version" "${flags[@]}" install
"$prefix/bin/git" version
