#!/usr/bin/env bash
# Records real gt output into internal/cli/testdata/gt, one JSON container per
# scenario holding the argv, both streams, and the exit code verbatim, beside an
# .md saying what produced them. Every scenario builds its own throwaway repo and
# its own HOME under a fixed work root, and git's identity and dates are pinned
# below, so two consecutive runs write an identical tree.
#
# Scenarios gt can reach with no token and no network record by default; the
# ones that need a Graphite token need --live and CCX_GT_RECORD_TOKEN. The rest
# — the ready line only a Graphite-permitted repo produces —
# get an .md saying what is missing, never bytes somebody guessed.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/internal/cli/testdata/gt"
work="${CCX_GT_RECORD_ROOT:-/tmp/ccx-gt-record}"

live=0
bump=0
for arg in "$@"; do
	case "$arg" in
	--live) live=1 ;;
	--bump) bump=1 ;;
	*)
		echo "record-gt-goldens: unknown argument $arg (want --live and/or --bump)" >&2
		exit 2
		;;
	esac
done

if ! command -v gt >/dev/null 2>&1; then
	echo "record-gt-goldens: gt is not on PATH" >&2
	exit 1
fi
version="$(gt --version)"

if [ -f "$dest/VERSION" ]; then
	pinned="$(cat "$dest/VERSION")"
	if [ "$pinned" != "$version" ] && [ "$bump" -eq 0 ]; then
		echo "record-gt-goldens: gt $version is not the pinned $pinned" >&2
		echo "  install the pin (npm i -g @withgraphite/graphite-cli@$pinned)," >&2
		echo "  or re-record every scenario at $version with: --bump --live" >&2
		exit 1
	fi
fi
if [ "$bump" -eq 1 ] && [ "$live" -eq 0 ]; then
	echo "record-gt-goldens: --bump re-pins VERSION for the whole tree, so it needs --live too" >&2
	exit 1
fi

token=""
if [ "$live" -eq 1 ]; then
	token="${CCX_GT_RECORD_TOKEN:-}"
	if [ -z "$token" ]; then
		echo "record-gt-goldens: --live needs CCX_GT_RECORD_TOKEN (a Graphite CLI token, as in ~/.config/graphite/auth)" >&2
		exit 1
	fi
fi

rm -rf "$work"
mkdir -p "$work"
work="$(cd "$work" && pwd -P)"
mkdir -p "$dest"

streams="$(mktemp -d)"
trap 'rm -rf "$streams"' EXIT

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=recorder GIT_AUTHOR_EMAIL=recorder@example.invalid
export GIT_COMMITTER_NAME=recorder GIT_COMMITTER_EMAIL=recorder@example.invalid
export GIT_AUTHOR_DATE="2020-01-01T00:00:00Z" GIT_COMMITTER_DATE="2020-01-01T00:00:00Z"

# prepare gives one scenario its own HOME, and a token or a reachable network
# only where the scenario is about having one. gt reads a token from
# GRAPHITE_AUTH_TOKEN and fetches feature flags at run time, so an offline
# scenario runs behind a proxy pointed at a closed port: measured at gt 1.8.6,
# that leaves every offline scenario's bytes untouched while making it
# impossible for an operator's environment to decide what they record.
#
#   offline  no token, no network — the default
#   net      no token, network reachable, for a refusal gt gets from the server
#   live     token and network, for the scenarios --live gates
prepare() {
	local mode=${2:-offline} home="$work/$1/home"
	mkdir -p "$home/.config/graphite"
	printf '{\n  "updateAutomatically": false\n}\n' >"$home/.config/graphite/user_config"
	export HOME="$home" XDG_CONFIG_HOME="$home/.config" XDG_STATE_HOME="$home/.state" XDG_CACHE_HOME="$home/.cache"
	unset GRAPHITE_AUTH_TOKEN
	case "$mode" in
	offline) export HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 ALL_PROXY=http://127.0.0.1:9 ;;
	net) unset HTTPS_PROXY HTTP_PROXY ALL_PROXY ;;
	live)
		umask 077
		printf '{"authToken":"%s"}' "$token" >"$home/.config/graphite/auth"
		umask 022
		unset HTTPS_PROXY HTTP_PROXY ALL_PROXY
		;;
	esac
}

# graphite_repo builds a git repo gt has initialized, with one commit on main,
# and chdirs into it. remote is a git URL for a scenario whose verb needs one.
graphite_repo() {
	local remote=${2:-} dir="$work/$1/repo"
	mkdir -p "$dir"
	cd "$dir"
	git init -q -b main .
	echo base >f.txt
	git add f.txt
	git commit -qm init
	if [ -n "$remote" ]; then
		git remote add origin "$remote"
	fi
	gt init --trunk main --no-interactive >/dev/null 2>&1
}

# tracked_branch commits content on a new branch gt tracks, then leaves the
# checkout wherever the caller asked.
tracked_branch() {
	local branch=$1 content=$2
	git switch -qc "$branch"
	printf '%s\n' "$content" >f.txt
	git commit -qam "$branch"
	gt track -f --no-interactive >/dev/null 2>&1
}

# tips_off silences gt's NUX tips for one scenario. Tips are unprefixed stderr,
# so a scenario about a severity line reads better without them; the ones that
# are about tips leave prepare's default (on) alone. This is the deterministic
# switch — never exhaust the nux showCount instead, since that is a counter every
# gt command in the same scenario also moves, which makes the captured bytes a
# function of the setup rather than of the verb. modify-tips-exit0.md says how.
tips_off() {
	printf '{\n  "updateAutomatically": false,\n  "tips": false\n}\n' >"$HOME/.config/graphite/user_config"
}

# capture runs gt in the current directory and writes the scenario's golden: the
# argv, both streams, and the exit status, as one JSON container
# (scripts/goldenjson.py, which says why the payloads are JSON strings).
capture() {
	local name=$1 code=0
	shift
	gt "$@" >"$streams/stdout" 2>"$streams/stderr" || code=$?
	python3 "$root/scripts/goldenjson.py" "$dest/$name.json" \
		--int "exit=$code" \
		--text "stdout=$streams/stdout" \
		--text "stderr=$streams/stderr" \
		--argv "$@"
	echo "record-gt-goldens: $name → exit $code"
}

# readme writes a scenario's sibling .md from stdin. Every scenario has one,
# whether or not it holds bytes.
readme() {
	cat >"$dest/$1.md"
}

# unrecordable declares a scenario nobody can record here: it keeps its .md
# alone, so the gap is visible in the tree instead of filled with invented
# output.
unrecordable() {
	rm -f "${dest:?}/$1.json"
	readme "$1"
	echo "record-gt-goldens: $1 → not recordable (.md only)"
}

scenario_auth_no_token() {
	prepare auth-no-token
	graphite_repo auth-no-token
	capture auth-no-token auth --no-interactive
	readme auth-no-token <<'EOF'
The lane's reachability probe with no Graphite token.

Recorded offline, no token. Exit 1. The argv is gtReachable's own. Pins
gtProbeNoToken ("No auth token set"), the one probe failure classifyGTProbe
answers with its own sentence rather than gt's line.
EOF
}

scenario_auth_authenticated_elsewhere() {
	prepare auth-authenticated-elsewhere live
	mkdir -p "$work/auth-authenticated-elsewhere/plain"
	cd "$work/auth-authenticated-elsewhere/plain"
	capture auth-authenticated-elsewhere auth --no-interactive
	readme auth-authenticated-elsewhere <<'EOF'
The lane's reachability probe outside any git repo, with a valid token.

Recorded live (CCX_GT_RECORD_TOKEN). gt confirms who you are and exits 0 without
ever confirming a repo is submittable. Pins the exit-0-is-not-consent branch of
classifyGTProbe: a yes needs gt's own ready line, so this answer is unknown.
EOF
}

scenario_auth_no_perms() {
	prepare auth-no-perms live
	graphite_repo auth-no-perms https://github.com/yasyf/cc-context.git
	capture auth-no-perms auth --no-interactive
	readme auth-no-perms <<'EOF'
The lane's reachability probe against a GitHub repo Graphite is not permitted to
submit to (this one).

Recorded live (CCX_GT_RECORD_TOKEN); reads Graphite's API, writes nothing. Exit
1 with gt's identity line on stdout and the refusal on stderr — the one recorded
scenario whose two streams both carry payload, so it is also gtJoinStreams'
golden. Pins gtProbeNoPerms, whose whole line classifyGTProbe quotes as the
lane's decline note, because that line names the repo.
EOF
}

scenario_auth_unreachable() {
	prepare auth-unreachable live
	graphite_repo auth-unreachable https://github.com/yasyf/cc-context.git
	export HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 ALL_PROXY=http://127.0.0.1:9
	capture auth-unreachable auth --no-interactive
	unset HTTPS_PROXY HTTP_PROXY ALL_PROXY
	readme auth-unreachable <<'EOF'
The lane's reachability probe with a valid token and Graphite's servers out of
reach — a proxy pointed at a closed port, so gt's own connection fails.

Recorded live (CCX_GT_RECORD_TOKEN): reaching the "cannot connect" branch takes
a token, since gt refuses for the missing one first. Pins gtProbeUnreadable, the
answer that leaves the verdict unknown rather than denied — a lane nobody could
confirm is not one to ride.
EOF
}

# modify_scenario records gt modify -c over a tracked branch feat. With clash, a
# tracked child rewrites the same line first and feat's new commit rewrites it
# again, so the restack gt modify runs afterwards cannot move child.
modify_scenario() {
	local name=$1 tips=$2 clash=$3
	prepare "$name"
	[ "$tips" = on ] || tips_off
	graphite_repo "$name"
	tracked_branch feat feat
	if [ "$clash" = clash ]; then
		tracked_branch child child
		git switch -q feat
		printf 'clash\n' >f.txt
	else
		printf 'more\n' >>f.txt
	fi
	git add f.txt
	capture "$name" modify -c -m "$name" --no-interactive
}

scenario_create_quiet_exit0() {
	prepare create-quiet-exit0
	tips_off
	graphite_repo create-quiet-exit0
	printf 'feat\n' >f.txt
	git add f.txt
	capture create-quiet-exit0 create feat -m create-quiet-exit0 --no-interactive
	readme create-quiet-exit0 <<'EOF'
The everyday gt create: a new stacked branch off trunk, tips off.

Recorded offline, no token. Exit 0, git's commit summary on stdout and stderr
empty. gt modify cannot stand in here: its modify.into tip prints even with tips
off.
EOF
}

scenario_modify_tips_exit0() {
	modify_scenario modify-tips-exit0 on plain
	readme modify-tips-exit0 <<'EOF'
gt modify -c on a branch with nothing above it, tips on.

Recorded offline, no token. Exit 0 with stderr non-empty and no severity line on
it: the negative case gtResult.Diagnostics' gate exists for, since gt's NUX tips
are unprefixed stderr exactly as a remediation is.

Tip bytes are a function of the whole scenario, not the captured command. gt
shows each nux a fixed number of times per HOME and records the count in
$XDG_DATA_HOME/graphite/nuxes, so the dots in a tip move whenever a gt command is
added to or removed from this scenario's setup. A dot mismatch on a re-record
means the setup changed; it is not licence to edit the expected bytes.
EOF
}

scenario_modify_decline_exit0() {
	modify_scenario modify-decline-exit0 off clash
	readme modify-decline-exit0 <<'EOF'
gt modify -c on a branch whose child then will not restack onto it, tips off.

Recorded offline, no token. Exit 0 with a WARNING: line, a blank line, gt's
unprefixed remediation, and the modify.into tip, which prints even with tips off.
It is why Diagnostics reports a severity-led stderr whole rather than the
severity lines alone: gt separates the remediation from its warning with the
same blank line it uses before the tip, so no window wide enough to keep the one
excludes the other.
EOF
}

unrecordable_scenarios() {
	unrecordable auth-ready <<'EOF'
NOT RECORDED. gt auth in a repo Graphite is permitted to submit to — the ready
line classifyGTProbe reads as the lane's one yes (gtProbeReady, "Ready to submit
PRs to").

What it needs: a token (CCX_GT_RECORD_TOKEN) and a checkout whose origin is a
GitHub repo synced with Graphite and permitted for that token. Measured
2026-08-02 at gt 1.8.6: no public repo this token reaches answers with the ready
line — every one of them answers with the gtProbeNoPerms refusal recorded under
auth-no-perms — so recording it would put a private repository's name in this
tree. Left unrecorded rather than guessed, and rather than disclosed by default.

To record it, point a scratch repo's origin at a permitted repo and run
`gt auth --no-interactive`, then add it here.
EOF

}

scenario_auth_no_token
scenario_create_quiet_exit0
scenario_modify_tips_exit0
scenario_modify_decline_exit0

if [ "$live" -eq 1 ]; then
	scenario_auth_authenticated_elsewhere
	scenario_auth_no_perms
	scenario_auth_unreachable
else
	echo "record-gt-goldens: skipped the live scenarios (--live + CCX_GT_RECORD_TOKEN records them)"
fi

unrecordable_scenarios

printf '%s\n' "$version" >"$dest/VERSION"
cat >"$dest/README.md" <<EOF
# Recorded \`gt\` goldens

gt resolves a repository through Graphite's API before it touches git, so its
network verbs cannot run in a test — but nothing here is hand-written. Every
byte came out of gt $version under \`scripts/record-gt-goldens.sh\`
(\`task record-gt\`), and a scenario nobody could record holds a README saying
what is missing instead of a plausible guess.

Re-record after a gt upgrade:

\`\`\`sh
task record-gt                    # the scenarios that need no token
task record-gt -- --live          # + the ones that do (CCX_GT_RECORD_TOKEN)
task record-gt -- --bump --live   # re-pin VERSION to a new gt and record it all
\`\`\`

\`VERSION\` holds the gt these came from, and the recorder refuses to run against
any other, so an upgrade is deliberate rather than a silent drift.

## Layout

Two files per scenario, \`<name>.json\` and \`<name>.md\`:

| File | Contents |
|---|---|
| \`<name>.json\` | \`argv\` (verb first), \`stdout\`, \`stderr\`, and \`exit\`, exactly as gt produced them |
| \`<name>.md\` | what produced the bytes, and which classifier they pin |

An \`<name>.md\` with no \`<name>.json\` beside it could not be recorded here.

The streams live inside JSON strings rather than as loose files because the
repo's commit hooks rewrite loose text — \`trailing-whitespace\` and
\`end-of-file-fixer\` between them strip and append the exact bytes these
goldens exist to preserve. Three scenarios end a \`stderr\` line with a space,
which is gt's own \`splog.error\` template appending one when called without a
second argument; a hook stripping it would make the golden a quiet lie about
what gt printed. JSON escaping puts the payload out of their reach, and
\`check-json\` validates the container for free.

## What moves between recordings

The work root the recorder builds its repos under (\`CCX_GT_RECORD_ROOT\`,
default \`/tmp/ccx-gt-record\`) lands in any output naming a path, so those
scenarios differ between a macOS and a Linux recording. gt also fetches feature
flags at run time, so a flag flip can reword a message at an unchanged version.
\`internal/cli/gtgolden_test.go\` walks every scenario and fails when recorded
bytes stop classifying, which is how a reword surfaces.
EOF
echo "record-gt-goldens: pinned $dest/VERSION to $version"
