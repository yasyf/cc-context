#!/usr/bin/env bash
set -euo pipefail

version=2.56.0
sha256=26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3
prefix="${1:?usage: build-git-loose-ref-control.sh <prefix> <baseline|candidate>}"
variant="${2:?usage: build-git-loose-ref-control.sh <prefix> <baseline|candidate>}"
case "$variant" in
	baseline|candidate) ;;
	*) exit 2 ;;
esac
work="$(mktemp -d "${RUNNER_TEMP:?}/git-loose-ref-build.XXXXXX")"
trap 'rm -rf "$work"' EXIT

sudo apt-get update -q
sudo apt-get install -y -q --no-install-recommends libcurl4-openssl-dev zlib1g-dev strace
tarball="$work/git-$version.tar.xz"
curl --fail --silent --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
	-o "$tarball" "https://kernel.org/pub/software/scm/git/git-$version.tar.xz"
echo "$sha256  $tarball" | shasum -a 256 -c -
tar -xJf "$tarball" -C "$work"
source="$work/git-$version"
echo "bfb57f4c3f4ff9428273106afa03296344b777db5b0de3390514db7d987cec4a  $source/odb/source-packed.c" | shasum -a 256 -c -
echo "fb30cd6fdcde7c02731a01f2533cf5e070cb557ee81809639c7446bf16416669  $source/refs/ref-cache.c" | shasum -a 256 -c -
echo "0fb289196da04b1c71bcd601234673f0ef7f83898ad40a0aeaa1beeb1d8a6808  $source/refs/ref-cache.h" | shasum -a 256 -c -
echo "430c0cc19e0355b8ebbc459d68fbd9eccc041d82d42b94718e079f0a9943b7ca  $source/refs/files-backend.c" | shasum -a 256 -c -
echo "5a0a9b4de308ddf75df9c6054760acebe6227d5178f8f348df945cc406dc6dab  $source/revision.c" | shasum -a 256 -c -
patch -d "$source" -p1 < "$GITHUB_WORKSPACE/scripts/git-loose-ref-baseline.patch"
patch -d "$source" -p1 < "$GITHUB_WORKSPACE/scripts/git-loose-ref-test-helper.patch"
echo "3721ce7276e9e96c860a89edc9265794b32d3f14d52b7b7f48e73f2104352294  $source/odb/source-packed.c" | shasum -a 256 -c -
if [ "$variant" = candidate ]; then
	patch -d "$source" -p1 < "$GITHUB_WORKSPACE/scripts/git-loose-ref-revision.patch"
	patch -d "$source" -p1 < "$GITHUB_WORKSPACE/scripts/git-loose-ref-cache.patch"
	echo "099b1dee0e02e0370943b7a228cf7b79b957f1d7749e365e80b72660e006793a  $source/refs/ref-cache.c" | shasum -a 256 -c -
	echo "5722be7c950be3d5099ebb3054cabf7f325fecdc173211d48fc546550bc83b8e  $source/refs/ref-cache.h" | shasum -a 256 -c -
	echo "3bb064d0aebe284295d641a453c472cba9a1b435f1b949f8716c68df883403ca  $source/refs/files-backend.c" | shasum -a 256 -c -
	echo "4adf45845ea7c967edcfe12d9267cc7a9a6a2e84af02fb87f0b6f97ef75bab39  $source/revision.c" | shasum -a 256 -c -
fi
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
if [ "$variant" = candidate ]; then
	for format in files reftable; do
		for hash in sha1 sha256; do
			GIT_TEST_DEFAULT_REF_FORMAT="$format" GIT_TEST_DEFAULT_HASH="$hash" \
				make -C "$source/t" -j2 t1400-update-ref.sh t1405-main-ref-store.sh t6300-for-each-ref.sh
		done
	done
fi
make -C "$source" "${flags[@]}" install
cp "$source/t/helper/test-tool" "$prefix/bin/git-loose-ref-test-tool"
"$prefix/bin/git" version
shasum -a 256 "$prefix/bin/git" "$prefix/bin/git-loose-ref-test-tool"
