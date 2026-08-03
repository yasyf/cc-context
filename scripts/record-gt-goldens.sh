#!/usr/bin/env bash
# Records real gt output into internal/cli/testdata/gt, one JSON container per
# scenario holding the argv, both streams, and the exit code verbatim, beside an
# .md saying what produced them. Every scenario builds its own throwaway repo and
# its own HOME under a fixed work root, and git's identity and dates are pinned
# below, so two consecutive runs write an identical tree.
#
# Scenarios gt can reach with no token and no network record by default; the
# ones that need a Graphite token need --live and CCX_GT_RECORD_TOKEN. The rest
# — a submit refusal only a Graphite-permitted repo with a live stack produces —
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

# graphite_identity points gt's repo resolution at a real GitHub repo while the
# git remote stays local, which is what lets a scenario reach the phases past
# resolution — the fast-forward and restack a sync-exit-0 shape needs. Without
# it Graphite 404s on a file:// remote and the run dies at exit 1, which is what
# sync-repo-404 records. Nothing is pushed to the named repo; gt only asks
# Graphite about it.
graphite_identity() {
	python3 - .git/.graphite_repo_config <<'PY'
import json, sys

path = sys.argv[1]
with open(path) as f:
    config = json.load(f)
config["owner"], config["name"] = "yasyf", "cc-context"
with open(path, "w") as f:
    json.dump(config, f, indent=2)
PY
}

# tips_off silences gt's NUX tips for one scenario. Tips are unprefixed stderr,
# so a scenario about a severity line reads better without them; the ones that
# are about tips leave prepare's default (on) alone. This is the deterministic
# switch — never exhaust the nux showCount instead, since that is a counter every
# gt command in the same scenario also moves, which makes the captured bytes a
# function of the setup rather than of the verb. sync-tips-exit0.md says how.
tips_off() {
	printf '{\n  "updateAutomatically": false,\n  "tips": false\n}\n' >"$HOME/.config/graphite/user_config"
}

# trunk_moved_under advances origin's trunk past the local one, so the next sync
# fast-forwards trunk and then restacks branch onto it.
trunk_moved_under() {
	local branch=$1
	git switch -q main
	printf 'trunk\n' >f.txt
	git commit -qam trunk
	git push -q origin main
	git reset --hard -q HEAD~1
	git switch -q "$branch"
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

scenario_restack_conflict() {
	prepare restack-conflict
	graphite_repo restack-conflict
	tracked_branch feat feat
	git switch -q main
	printf 'trunk\n' >f.txt
	git commit -qam trunk
	git switch -q feat
	capture restack-conflict restack --no-interactive
	readme restack-conflict <<'EOF'
gt restack over a branch whose commit conflicts with trunk's.

Recorded offline, no token. gt writes the conflict banner to stdout and exits 1.
Pins gtSyncConflict ("Hit conflict restacking"), the sentence classifyGTRestack
turns into the gt continue / gt abort advice. gt sync prints the same banner
when its restack phase conflicts, and sync cannot run offline, so restack is
where the wording is recorded.
EOF
}

scenario_restack_blocked_during_rebase() {
	prepare restack-blocked-during-rebase
	graphite_repo restack-blocked-during-rebase
	tracked_branch feat feat
	git switch -q main
	printf 'trunk\n' >f.txt
	git commit -qam trunk
	git switch -q feat
	gt restack --no-interactive >/dev/null 2>&1 || true
	capture restack-blocked-during-rebase restack --no-interactive
	readme restack-blocked-during-rebase <<'EOF'
gt restack run a second time, with the conflicted rebase from the first still
open.

Recorded offline, no token. Exit 1 with an ERROR:-led diagnostic on stderr and
nothing on stdout. Pins an unrecognized failure — classifyGTRestack wraps it
verbatim — and the ERROR: prefix Diagnostics and reportedError read.
EOF
}

scenario_restack_worktree_held() {
	prepare restack-worktree-held
	graphite_repo restack-worktree-held
	tracked_branch feat feat
	git switch -q main
	git worktree add -q "$work/restack-worktree-held/held" feat
	printf 'trunk\n' >>f.txt
	git commit -qam trunk
	capture restack-worktree-held restack --no-interactive
	readme restack-worktree-held <<'EOF'
gt restack with the stack's branch checked out in a second worktree.

Recorded offline, no token. gt declines the branch, says so on stdout, and still
exits 0 — the exit-0-that-did-nothing case gtZeroSurfaces exists for. Pins
gtSyncSkippedPrefix / gtSyncSkippedReason / gtSyncSkippedWorktree, which
gtSyncSkipped cuts a branch and its reason out of, and pins that the line
carries no ERROR: prefix, so reportedError stays false.

The recorded path is the recorder's work root (CCX_GT_RECORD_ROOT, default
/tmp/ccx-gt-record) as git resolves it, so it differs between a macOS and a
Linux recording. Nothing reads the path itself; the tests read the shape.
EOF
}

scenario_restack_frozen() {
	prepare restack-frozen
	graphite_repo restack-frozen
	tracked_branch feat feat
	gt freeze --no-interactive >/dev/null 2>&1
	git switch -q main
	printf 'trunk\n' >>f.txt
	git commit -qam trunk
	git switch -q feat
	capture restack-frozen restack --no-interactive
	readme restack-frozen <<'EOF'
gt restack over a frozen branch (gt freeze).

Recorded offline, no token. The second reason gt gives for declining a branch at
exit 0, and the one with no trailing path: gtSyncSkipped renders it as the bare
word gt used.
EOF
}

scenario_sync_no_remote() {
	prepare sync-no-remote
	graphite_repo sync-no-remote
	capture sync-no-remote sync --no-interactive
	readme sync-no-remote <<'EOF'
gt sync in a repo with no git remote.

Recorded offline, no token. Exit 1: gt resolves the repo through its own API
before touching git, so with no remote to name the repo it never gets that far.
Pins an unrecognized sync failure — classifyGTRestack wraps it verbatim — plus
the tip block gt writes to stderr ahead of its ERROR: line.
EOF
}

scenario_sync_auth_invalid() {
	prepare sync-auth-invalid net
	git init -q --bare "$work/sync-auth-invalid/bare.git"
	graphite_repo sync-auth-invalid "file://$work/sync-auth-invalid/bare.git"
	git push -q origin main
	capture sync-auth-invalid sync --no-interactive
	readme sync-auth-invalid <<'EOF'
gt sync in a repo with a remote, with no Graphite token.

Recorded with no token but a reachable network — the answer comes from
Graphite's server, so recording this one behind the offline proxy would capture
the connection failure instead. Exit 1. Pins gtSyncAuthRequired2 ("Your Graphite
auth token is invalid/expired"), which classifyGTRestack turns into the gt auth
advice; gt words a missing token that way once a remote exists to ask about.
EOF
}

scenario_submit_unauth() {
	prepare submit-unauth
	graphite_repo submit-unauth
	capture submit-unauth submit --no-interactive --no-edit --no-ai --no-stack --publish
	readme submit-unauth <<'EOF'
gt submit with no Graphite token.

Recorded offline, no token. Exit 1. The argv is gtSubmitArgv's own, so the
golden pins the flags ccx passes as well as the refusal it gets back. Pins
gtAuthRequired1 ("Please authenticate your Graphite CLI"), which classifyGTSubmit
turns into the gt auth advice.
EOF
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

scenario_submit_repo_unverified() {
	prepare submit-repo-unverified live
	git init -q --bare "$work/submit-repo-unverified/bare.git"
	graphite_repo submit-repo-unverified "file://$work/submit-repo-unverified/bare.git"
	git push -q origin main
	tracked_branch feat feat
	capture submit-repo-unverified submit --no-interactive --no-edit --no-ai --no-stack --publish
	readme submit-repo-unverified <<'EOF'
gt submit with a valid token against a repo Graphite cannot resolve — the remote
is a local bare repo, so nothing is pushed anywhere.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 1, with a WARNING: line ahead of the
ERROR:. This is what classifyGTSubmit's default arm faces: a real refusal in
none of the recognized wordings, wrapped verbatim. It also shows why the
recognized submit refusals below cannot be recorded — Graphite resolves the repo
through its API before gt looks at the stack, so a refusal about the stack is
downstream of a repo Graphite already accepted.

gt names the repo it could not verify after the bare remote's path, so these
bytes carry the recorder's work root (CCX_GT_RECORD_ROOT, default
/tmp/ccx-gt-record) and move with it.
EOF
}

scenario_sync_repo_404() {
	prepare sync-repo-404 live
	git init -q --bare "$work/sync-repo-404/bare.git"
	graphite_repo sync-repo-404 "file://$work/sync-repo-404/bare.git"
	git push -q origin main
	capture sync-repo-404 sync --no-interactive
	readme sync-repo-404 <<'EOF'
gt sync with a valid token against a repo Graphite cannot resolve — the remote is
a local bare repo, so nothing is fetched from anywhere.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 1. classifyGTRestack's default arm: a
real failure in neither the conflict nor the auth wording, wrapped verbatim.
EOF
}

scenario_sync_decline_exit0() {
	prepare sync-decline-exit0 live
	tips_off
	git init -q --bare "$work/sync-decline-exit0/bare.git"
	graphite_repo sync-decline-exit0 "file://$work/sync-decline-exit0/bare.git"
	graphite_identity
	git push -q origin main
	tracked_branch feat feat
	trunk_moved_under feat
	capture sync-decline-exit0 sync --no-interactive
	readme sync-decline-exit0 <<'EOF'
gt sync whose trunk fast-forwards and whose branch then will not restack onto
it, tips off.

Recorded live (CCX_GT_RECORD_TOKEN). **Exit 0**, and the shape gtZeroSurfaces
and restack.go's zero-surfaces path exist for: stdout's restack section is
empty, so the only account of what happened is on stderr. This is the trigger
case for gtResult.Diagnostics — a WARNING: line, a blank line, and gt's
unprefixed remediation, which is why the echo reports stderr whole rather than
the severity lines alone.

The remote is a local bare repo; the owner/name in .graphite_repo_config points
Graphite's resolution at a real repository so the run reaches the fast-forward
phase at all. Nothing is pushed there.
EOF
}

scenario_sync_decline_unstaged_exit0() {
	prepare sync-decline-unstaged-exit0 live
	tips_off
	git init -q --bare "$work/sync-decline-unstaged-exit0/bare.git"
	graphite_repo sync-decline-unstaged-exit0 "file://$work/sync-decline-unstaged-exit0/bare.git"
	graphite_identity
	git push -q origin main
	git switch -qc feat
	echo a >a.txt
	git add a.txt
	git commit -qm feat
	gt track -f --no-interactive >/dev/null 2>&1
	trunk_moved_under feat
	printf 'dirty\n' >f.txt
	capture sync-decline-unstaged-exit0 sync --no-interactive
	readme sync-decline-unstaged-exit0 <<'EOF'
gt sync whose checked-out branch needs a restack and carries a conflicting
unstaged edit, tips off.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 0, two WARNING: lines: gt names the
working-tree cause as well as the outcome, where sync-decline-exit0's committed
conflict produces only the outcome. Same unprefixed remediation below both.
This is the two-warning shape restack_test.go asserts on.
EOF
}

scenario_sync_tips_exit0() {
	prepare sync-tips-exit0 live
	git init -q --bare "$work/sync-tips-exit0/bare.git"
	graphite_repo sync-tips-exit0 "file://$work/sync-tips-exit0/bare.git"
	graphite_identity
	git push -q origin main
	tracked_branch feat feat
	git switch -q main
	capture sync-tips-exit0 sync --no-interactive
	readme sync-tips-exit0 <<'EOF'
A clean no-op gt sync with tips on: nothing to fast-forward, nothing to restack.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 0 with **stderr non-empty and no
severity line on it** — the negative case gtResult.Diagnostics' gate exists for.
Without that gate this stderr would be reported as a diagnostic on every
ordinary ship, because gt's NUX tips are unprefixed stderr exactly as the
remediation is. Pair with sync-quiet-exit0, the same sync with tips off.

TIP BYTES ARE A FUNCTION OF THE WHOLE SCENARIO, NOT THE CAPTURED COMMAND. gt
shows each nux a fixed number of times per HOME and records the count in
$XDG_DATA_HOME/graphite/nuxes: init.welcome and tip.expert-message cap at 1 and
are both spent by the gt init graphite_repo runs, runner.undo caps at 3. So the
dots in [runner.undo ●○○] say "first showing", and tip.expert-message is absent
here because gt init already used its only one. Add or remove a gt command
anywhere in this scenario's setup and the dots move. A dot mismatch on a
re-record means the setup changed — it is not licence to edit the expected
bytes.
EOF
}

scenario_sync_tips_and_warning_exit0() {
	prepare sync-tips-and-warning-exit0 live
	git init -q --bare "$work/sync-tips-and-warning-exit0/bare.git"
	graphite_repo sync-tips-and-warning-exit0 "file://$work/sync-tips-and-warning-exit0/bare.git"
	graphite_identity
	git push -q origin main
	tracked_branch feat feat
	trunk_moved_under feat
	capture sync-tips-and-warning-exit0 sync --no-interactive
	readme sync-tips-and-warning-exit0 <<'EOF'
gt sync that declines a restack with tips on, so both classes of unprefixed
stderr land in one capture.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 0. The hard case for the echo's gate:
a severity line is present, so the whole block is reported, tips included. It is
also the proof that no rule short of the gate works — gt separates the
remediation from its warning with the same "\n\n" it uses to separate the
remediation from the tip block that follows, so any window wide enough to keep
the first crosses the second.

The tip-dot caveat on sync-tips-exit0 applies here too.
EOF
}

scenario_sync_quiet_exit0() {
	prepare sync-quiet-exit0 live
	tips_off
	git init -q --bare "$work/sync-quiet-exit0/bare.git"
	graphite_repo sync-quiet-exit0 "file://$work/sync-quiet-exit0/bare.git"
	graphite_identity
	git push -q origin main
	tracked_branch feat feat
	git switch -q main
	capture sync-quiet-exit0 sync --no-interactive
	readme sync-quiet-exit0 <<'EOF'
The everyday sync: nothing to do, tips off, stderr empty.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 0 with both streams saying only that
there was nothing to do. It is the control for sync-tips-exit0 — same repo
state, same argv, tips the only difference — which is what isolates the 573
bytes that scenario puts on stderr as tips rather than as anything gt had to
report.
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

	unrecordable submit-restack-needed <<'EOF'
NOT RECORDED. gt submit refusing a stack that drifted (gtRestackNeeded1 "You
must restack before submitting this stack." / gtRestackNeeded2 "You must restack
and resolve conflicts with "), which classifyGTSubmit turns into the gt restack
advice.

What it needs: a token plus a checkout of a GitHub repo Graphite is permitted to
submit to, holding a tracked branch whose parent has moved. Measured 2026-08-02
at gt 1.8.6: gt resolves the repository through Graphite's API before it looks
at the stack, so against any repo reachable here the run fails at resolution
instead (see submit-repo-unverified). Recording it against a permitted repo
would submit a real pull request, which is why it is not done from a recorder.
EOF

	unrecordable submit-trunk-stale <<'EOF'
NOT RECORDED. gt submit refusing because trunk is behind (gtTrunkStale
"Aborting submit because trunk branch is out of date"), which classifyGTSubmit
turns into the gt sync advice.

What it needs: the same permitted repo as submit-restack-needed, with the remote
trunk ahead of the local one. Same blocker: reachable only past Graphite's
repository resolution, and a successful path would push.
EOF

	unrecordable submit-remote-changed <<'EOF'
NOT RECORDED. gt submit refusing because the remote branch moved
(gtRemoteChanged1 "This branch has been updated remotely since you last
submitted" / gtRemoteChanged2 "Force-with-lease push failed due to external
changes to the remote branch"), which classifyGTSubmit turns into the reconcile
advice.

What it needs: a permitted repo with a branch already submitted once and since
changed on the remote. Same blocker as submit-restack-needed, and reproducing it
means two real submits.
EOF

	unrecordable sync-conflict <<'EOF'
NOT RECORDED. gt sync hitting a conflict while restacking after it pulls trunk.

What it needs: a token and a permitted repo whose trunk moved under a stack that
conflicts with it. gt sync cannot get past repository resolution offline, so the
banner is recorded from gt restack instead — see restack-conflict, which carries
the same gtSyncConflict sentence and is what classifyGTRestack matches.
EOF

}

scenario_restack_conflict
scenario_restack_blocked_during_rebase
scenario_restack_worktree_held
scenario_restack_frozen
scenario_sync_no_remote
scenario_sync_auth_invalid
scenario_submit_unauth
scenario_auth_no_token

if [ "$live" -eq 1 ]; then
	scenario_auth_authenticated_elsewhere
	scenario_auth_no_perms
	scenario_auth_unreachable
	scenario_submit_repo_unverified
	scenario_sync_repo_404
	scenario_sync_decline_exit0
	scenario_sync_decline_unstaged_exit0
	scenario_sync_tips_exit0
	scenario_sync_tips_and_warning_exit0
	scenario_sync_quiet_exit0
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
goldens exist to preserve. Nine scenarios end a \`stderr\` line with a space,
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
