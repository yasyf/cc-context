#!/usr/bin/env bash
# Prints one shard's anchored -run regex for internal/cli, or every shard's with
# --all. --check proves the shards partition it: a test in no shard never runs
# and CI stays green.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
pkg=./internal/cli/
weights="$root/internal/cli/testdata/shard-weights.json"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

usage() {
	echo "usage: shard-tests.sh <index> <total> | shard-tests.sh --all <total> | shard-tests.sh --check <total>" >&2
	exit 2
}

# SHARD_TEST_BIN reuses an already-compiled test binary, so CI can bin the
# shards without paying a second compile to enumerate the tests.
list() {
	if [ -n "${SHARD_TEST_BIN:-}" ]; then
		(cd "$root/internal/cli" && "$SHARD_TEST_BIN" -test.list "$1")
	else
		(cd "$root" && go test -list "$1" "$pkg")
	fi | grep -E '^Test' | sort
}

partition() {
	local total="$1"
	list '.*' |
		awk -v FS='\t' 'NR == FNR { weight[$1] = $2; next } { printf "%d\t%s\n", ($0 in weight ? weight[$0] : -1), $0 }' \
			<(jq -r 'to_entries[] | "\(.key)\t\(.value)"' "$weights") - |
		sort -k1,1nr -k2,2 |
		awk -v total="$total" '
			BEGIN { for (i = 0; i < total; i++) load[i] = 0 }
			{
				if ($1 >= 0) {
					best = 0
					for (i = 1; i < total; i++) if (load[i] < load[best]) best = i
					load[best] += $1
				} else {
					best = roundRobin % total
					roundRobin++
				}
				names[best] = names[best] == "" ? $2 : names[best] "|" $2
			}
			END { for (i = 0; i < total; i++) printf "%d\t^(%s)$\n", i, names[i] }
		'
}

check() {
	local total="$1" regex
	: >"$tmp/union"
	while IFS=$'\t' read -r _ regex; do
		list "$regex" >>"$tmp/union"
	done < <(partition "$total")
	list '.*' >"$tmp/all"
	if ! diff -u "$tmp/all" <(sort -u "$tmp/union"); then
		echo "shard-tests: the $total shards do not cover internal/cli" >&2
		exit 1
	fi
	if [ "$(wc -l <"$tmp/union")" -ne "$(wc -l <"$tmp/all")" ]; then
		echo "shard-tests: a test lands in more than one of the $total shards" >&2
		exit 1
	fi
	echo "shard-tests: $total shards partition $(wc -l <"$tmp/all" | tr -d ' ') tests"
}

if [ "${1:-}" = "--check" ]; then
	[ $# -eq 2 ] || usage
	check "$2"
	exit 0
fi

if [ "${1:-}" = "--all" ]; then
	[ $# -eq 2 ] || usage
	partition "$2"
	exit 0
fi

[ $# -eq 2 ] || usage
[ "$1" -ge 0 ] && [ "$1" -lt "$2" ] || usage
partition "$2" | awk -v FS='\t' -v want="$1" '$1 == want { print $2 }'
