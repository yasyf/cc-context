#!/usr/bin/env bash
# Records real `gh` output into internal/cli/testdata/gh, so the gh fakes replay
# bytes GitHub actually produced instead of bytes someone hand-wrote.
#
#   scripts/record-gh-goldens.sh                  # every read scenario
#   scripts/record-gh-goldens.sh --only run-      # scenarios whose name matches
#   scripts/record-gh-goldens.sh --write OWNER/N  # + the pr create/edit/ready verbs
#
# Read scenarios run against public repositories (yasyf/cc-context and cli/cli);
# the write scenarios need a scratch repository they may create a pull request in.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/cli/testdata/gh"

own_repo="yasyf/cc-context"
foreign_repo="cli/cli"
foreign_owner="${foreign_repo%%/*}"
foreign_name="${foreign_repo##*/}"
# Two pull requests in $foreign_repo: an open one carrying inline comments, issue
# comments, and a CHANGES_REQUESTED review, and a merged one whose mergedAt is
# the non-null case reviewTerminalState reads.
open_pr=13982
open_pr_branch="o1/add-latest-pre-release-and-pin-flags-to-gh-extension-upgrade/nysoxynolqlo"
draft_pr_branch="maxbeizer-fix-project-scope-error"
merged_pr=13084
# A pull request whose inline feed carries a comment on an outdated diff, which
# is how GitHub produces the null "line" that ghPRComment holds as *int.
outdated_pr=14003
# Two branches of $own_repo: one carrying a single pull request, one carrying two
# (the descending-order case downstackPRQuery and reviewsBranchQuery both pick).
own_branch_one="fix-ship-help-graphite-demote"
own_branch_two="yasyf/transcript-ccx-issues"
own_sha="8ce0dcf1c1b66a60e890985c77a52064c6cfcb49"
# A public issue comment of $foreign_repo, for the by-id fetch ccx vcs status
# reads a Graphite merge-queue activity comment through.
foreign_comment="IC_kwDODKw3uc8AAAABM0KrfA"
run_success=30744524405
run_failed=30270014111
run_log_failed=30223463656

only=""
scratch=""
while [ $# -gt 0 ]; do
	case "$1" in
	--only)
		only="$2"
		shift 2
		;;
	--write)
		scratch="$2"
		shift 2
		;;
	*)
		echo "record-gh-goldens: unknown argument $1" >&2
		exit 2
		;;
	esac
done

command -v gh >/dev/null 2>&1 || {
	echo "record-gh-goldens: gh is not on PATH" >&2
	exit 1
}

version="$(gh --version | head -n1)"
if [ -n "$only" ] && [ -f "$out/VERSION" ] && [ "$(cat "$out/VERSION")" != "$version" ]; then
	echo "record-gh-goldens: $out/VERSION says $(cat "$out/VERSION"), gh is $version" >&2
	echo "record-gh-goldens: a partial re-record would mix versions — re-run without --only" >&2
	exit 1
fi

# An isolated config dir keeps a local gh alias, pager, or host override out of
# the recording; the token is the one gh already holds, since these scenarios
# read a private-membership-free surface but still need an authenticated viewer.
token="$(gh auth token)"
config="$(mktemp -d)"
streams="$(mktemp -d)"
trap 'rm -rf "$config" "$streams"' EXIT
export GH_CONFIG_DIR="$config"
export GH_TOKEN="$token"
export GH_PAGER=cat
export GH_PROMPT_DISABLED=1
export GH_NO_UPDATE_NOTIFIER=1
export NO_COLOR=1
unset GITHUB_TOKEN GH_HOST GH_REPO GH_FORCE_TTY 2>/dev/null || true

wanted() {
	[ -z "$only" ] || case "$1" in *"$only"*) return 0 ;; *) return 1 ;; esac
}

# record runs gh with the given argv and writes what a PATH fake replays: the
# argv, stdout, stderr, and the exit status, as one JSON container per scenario
# (scripts/goldenjson.py, which says why the payloads are JSON strings).
record() {
	local name="$1"
	shift
	wanted "$name" || return 0
	set +e
	(cd "$root" && gh "$@") >"$streams/stdout" 2>"$streams/stderr"
	local code=$?
	set -e
	python3 "$root/scripts/goldenjson.py" "$out/cli/$name.json" \
		--int "exit=$code" \
		--text "stdout=$streams/stdout" \
		--text "stderr=$streams/stderr" \
		--argv "$@"
	echo "recorded cli/$name (exit $code, $(wc -c <"$streams/stdout" | tr -d ' ') bytes)"
}

# record_http runs gh api -i and writes what an httptest replay serves: the
# status, the response headers a replay sets, and the body byte for byte.
# internal/ghapi speaks HTTP directly, so its goldens are responses rather than
# gh stdout.
#
# Only the headers a parser reads are kept. Everything else GitHub sends is
# per-request noise (Date, request ids, edge region) that would rewrite every
# golden on every recording without any parser ever reading it; the rate-limit
# headers retryDelay reads are kept only on a 403 or 429, where they carry the
# verdict rather than a running count.
record_http() {
	local name="$1"
	shift
	wanted "$name" || return 0
	set +e
	(cd "$root" && gh api -i "$@") >"$streams/raw" 2>"$streams/stderr"
	local code=$?
	set -e
	DIR="$streams" python3 - <<-'PY'
		import os, pathlib

		always = ("content-type", "link", "etag")
		limited = always + ("retry-after", "x-ratelimit-remaining", "x-ratelimit-reset")

		out = pathlib.Path(os.environ["DIR"])
		head, _, body = (out / "raw").read_bytes().partition(b"\r\n\r\n")
		lines = head.split(b"\r\n")
		status = lines[0].split(b" ")[1].decode()
		keep = limited if status in ("403", "429") else always
		headers = [line for line in lines[1:] if line.split(b":")[0].decode().lower() in keep]

		(out / "status").write_text(status + "\n")
		(out / "headers").write_bytes(b"".join(line + b"\n" for line in headers))
		(out / "body").write_bytes(body)
	PY
	python3 "$root/scripts/goldenjson.py" "$out/api/$name.json" \
		--int "exit=$code" \
		--int "status=$(cat "$streams/status")" \
		--lines "headers=$streams/headers" \
		--text "body=$streams/body" \
		--text "stderr=$streams/stderr" \
		--argv api -i "$@"
	echo "recorded api/$name (HTTP $(cat "$streams/status"), $(wc -c <"$streams/body" | tr -d ' ') bytes)"
}

viewer_query='query={viewer{login organizations(first:1){nodes{login}}}}'
guidelines_fields='nameWithOwner,pullRequestTemplates,codeOfConduct,contactLinks,issueTemplates'
repo_fields='nameWithOwner,owner,isPrivate,viewerPermission'
run_fields='workflowName,conclusion,startedAt,updatedAt,url,jobs'
raw_accept='Accept: application/vnd.github.raw'

# landing_fields is internal/cli/prlanding.go's prLandingFields, the selection
# that tells a pull request the graphite merge queue landed — closed, with a null
# mergedAt — from one a human abandoned.
landing_fields='state mergedAt timelineItems(last: 1, itemTypes: [CLOSED_EVENT]) { nodes { ... on ClosedEvent { actor { login } } } }'
checks_fields='commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }'

downstack_query() {
	local n="$1" decls fields i
	decls='$owner: String!, $repo: String!'
	fields=''
	for ((i = 0; i < n; i++)); do
		decls="$decls, \$b$i: String!"
		fields="$fields    b$i: pullRequests(headRefName: \$b$i, first: 1, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number url body $landing_fields $checks_fields } }
"
	done
	printf 'query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}' "$decls" "$fields"
}

# status_pr_fields is internal/cli/vcsstatusgh.go's statusPRFields, and
# status_protection_fields its statusProtectionFields.
# TestVcsStatusArgvIsTheRecordedOne fails if either drifts from the Go.
status_pr_fields="number url body isDraft baseRefName headRefOid mergeable mergeStateStatus reviewDecision $landing_fields files(first: 1) { totalCount } labels(first: 20) { nodes { name } } latestOpinionatedReviews(first: 20) { nodes { state author { login __typename } commit { oid } } } history: commits(last: 50) { nodes { commit { oid committedDate messageHeadline } } } checks: commits(last: 1) { nodes { commit { statusCheckRollup { state contexts(first: 100) { nodes { __typename ... on CheckRun { name conclusion status } ... on StatusContext { context state } } } } } } } events: timelineItems(last: 100, itemTypes: [LABELED_EVENT, UNLABELED_EVENT, HEAD_REF_FORCE_PUSHED_EVENT]) { nodes { __typename ... on LabeledEvent { createdAt label { name } actor { login } } ... on UnlabeledEvent { createdAt label { name } actor { login } } ... on HeadRefForcePushedEvent { createdAt beforeCommit { oid } } } } comments(last: 40) { nodes { id author { login } } }"
status_protection_fields='branchProtectionRules(first: 20) { nodes { pattern requiredStatusChecks { context } } }'

status_query() {
	local n="$1" decls fields i
	decls='$owner: String!, $repo: String!'
	fields=''
	for ((i = 0; i < n; i++)); do
		decls="$decls, \$b$i: String!"
		fields="$fields    b$i: pullRequests(headRefName: \$b$i, first: 1, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { ...prStatus } }
"
	done
	printf 'query(%s) {\n  repository(owner: $owner, name: $repo) {\n    %s\n%s  }\n}\nfragment prStatus on PullRequest { %s }' \
		"$decls" "$status_protection_fields" "$fields" "$status_pr_fields"
}

status_comment_query() {
	local n="$1" decls vars i
	decls=''
	vars=''
	for ((i = 0; i < n; i++)); do
		[ "$i" -eq 0 ] || { decls="$decls, "; vars="$vars, "; }
		decls="$decls\$c$i: ID!"
		vars="$vars\$c$i"
	done
	printf 'query(%s) { nodes(ids: [%s]) { ... on IssueComment { id body } } }' "$decls" "$vars"
}

status_draft_query() {
	local n="$1" decls fields i
	decls='$owner: String!, $repo: String!'
	fields=''
	for ((i = 0; i < n; i++)); do
		decls="$decls, \$d$i: Int!"
		fields="$fields    d$i: pullRequest(number: \$d$i) { number createdAt }
"
	done
	printf 'query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}' "$decls" "$fields"
}

reviews_query() {
	local kind="$1" n="$2" decls fields i decl
	case "$kind" in
	number) decl='Int!' ;;
	branch) decl='String!' ;;
	esac
	decls='$owner: String!, $repo: String!'
	fields=''
	for ((i = 0; i < n; i++)); do
		decls="$decls, \$p$i: $decl"
		case "$kind" in
		number) fields="$fields    p$i: pullRequest(number: \$p$i) { number url $landing_fields }
" ;;
		branch) fields="$fields    p$i: pullRequests(headRefName: \$p$i, first: 1, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number url $landing_fields } }
" ;;
		esac
	done
	printf 'query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}' "$decls" "$fields"
}

### vcs.LookupRepo — gh repo view + the viewer graphql

record repo-view-own repo view --json "$repo_fields"
record repo-view-foreign repo view "$foreign_repo" --json "$repo_fields"
record repo-view-missing repo view "$own_repo-does-not-exist" --json "$repo_fields"
record viewer-graphql api graphql -f "$viewer_query"

### ccx vcs guidelines — repo view + community profile + raw contents

record guidelines-repo-view-bare repo view --json "$guidelines_fields"
record guidelines-repo-view-populated repo view "$foreign_repo" --json "$guidelines_fields"
record guidelines-profile-none api "repos/$own_repo/community/profile"
record guidelines-profile-found api "repos/$foreign_repo/community/profile"
if wanted guidelines-contributing-raw; then
	contributing_url="$(gh api "repos/$foreign_repo/community/profile" --jq '.files.contributing.url')"
	record guidelines-contributing-raw api "$contributing_url" -H "$raw_accept"
fi

### ship's pull request lookup — gh pr list

record pr-list-empty pr list --repo "$own_repo" --head no-such-branch --state open --json number,url,isDraft --limit 1
record pr-list-found pr list --repo "$foreign_repo" --head "$open_pr_branch" --state open --json number,url,isDraft --limit 1
record pr-list-draft pr list --repo "$foreign_repo" --head "$draft_pr_branch" --state open --json number,url,isDraft --limit 1

### ccx vcs info's downstack — one batched graphql per stack

record downstack-graphql-one api graphql \
	-F 'owner={owner}' -F 'repo={repo}' \
	-f "b0=$own_branch_one" -f "query=$(downstack_query 1)"
record downstack-graphql-three api graphql \
	-F 'owner={owner}' -F 'repo={repo}' \
	-f "b0=$own_branch_one" -f "b1=$own_branch_two" -f "b2=no-such-branch" \
	-f "query=$(downstack_query 3)"

### ccx vcs status — the batched stack query, then the queue's own comment

record status-graphql-one api graphql \
	-F 'owner={owner}' -F 'repo={repo}' \
	-f "b0=$own_branch_one" -f "query=$(status_query 1)"
record status-graphql-three api graphql \
	-F 'owner={owner}' -F 'repo={repo}' \
	-f "b0=$own_branch_one" -f "b1=$own_branch_two" -f "b2=no-such-branch" \
	-f "query=$(status_query 3)"
record status-comment-graphql api graphql \
	-f "c0=$foreign_comment" -f "query=$(status_comment_query 1)"
record status-draft-graphql api graphql \
	-F "owner=$foreign_owner" -F "repo=$foreign_name" \
	-F "d0=$open_pr" -F "d1=$merged_pr" -f "query=$(status_draft_query 2)"

### ship's CI watch — gh run list / run view / run view --log-failed

record run-list run list --commit "$own_sha" --limit 50 --json databaseId,workflowName,status,url
record run-list-none run list --commit 0000000000000000000000000000000000000000 --limit 50 --json databaseId,workflowName,status,url
record run-view-success run view "$run_success" --json "$run_fields"
record run-view-failed run view "$run_failed" --json "$run_fields"
record run-log-failed run view "$run_log_failed" --log-failed

### ccx vcs reviews — internal/ghapi speaks HTTP, so these are responses

record_http reviews-graphql-numbers graphql \
	-F "owner=$foreign_owner" -F "repo=$foreign_name" \
	-F "p0=$open_pr" -F "p1=$merged_pr" -f "query=$(reviews_query number 2)"
record_http reviews-graphql-missing graphql \
	-F "owner=$foreign_owner" -F "repo=$foreign_name" \
	-F "p0=99999999" -f "query=$(reviews_query number 1)"
record_http reviews-graphql-branches graphql \
	-F "owner=$foreign_owner" -F "repo=$foreign_name" \
	-f "p0=$open_pr_branch" -f "p1=no-such-branch" -f "query=$(reviews_query branch 2)"

record_http reviews-inline-comments "repos/$foreign_repo/pulls/$open_pr/comments?per_page=100"
record_http reviews-inline-comments-outdated "repos/$foreign_repo/pulls/$outdated_pr/comments?per_page=100"
record_http reviews-issue-comments "repos/$foreign_repo/issues/$open_pr/comments?per_page=100"
record_http reviews-reviews "repos/$foreign_repo/pulls/$open_pr/reviews?per_page=100"
record_http reviews-inline-comments-empty "repos/$own_repo/pulls/2/comments?per_page=100"
record_http reviews-inline-comments-since "repos/$foreign_repo/pulls/$open_pr/comments?per_page=100&since=2099-01-01T00:00:00Z"
record_http reviews-pull-missing "repos/$own_repo/pulls/999999"
# per_page=1 over a three-review feed records the Link rel="next" chain Paginate
# walks. Production asks for per_page=100, which these feeds answer in one page.
record_http reviews-paginate-page1 "repos/$foreign_repo/pulls/$open_pr/reviews?per_page=1"
record_http reviews-paginate-page2 "repos/$foreign_repo/pulls/$open_pr/reviews?per_page=1&page=2"
record_http reviews-paginate-page3 "repos/$foreign_repo/pulls/$open_pr/reviews?per_page=1&page=3"

### ship's pull request writes — a scratch repository, opt-in

if [ -n "$scratch" ]; then
	work="$(mktemp -d)"
	branch="ccx-goldens-$(date -u +%Y%m%d%H%M%S)"
	if ! gh repo view "$scratch" >/dev/null 2>&1; then
		gh repo create "$scratch" --private --add-readme
	fi
	gh repo clone "$scratch" "$work/repo" -- --quiet
	(
		cd "$work/repo"
		trunk="$(gh repo view "$scratch" --json defaultBranchRef --jq .defaultBranchRef.name)"
		git checkout -q -b "$branch"
		date -u >goldens.txt
		git add goldens.txt
		git -c user.email=ccx@example.com -c user.name=ccx commit -q -m "record gh write goldens"
		git push -q origin "$branch"
		printf 'recorded by scripts/record-gh-goldens.sh\n' >"$work/body.md"
		record pr-create pr create --repo "$scratch" --head "$branch" --base "$trunk" \
			--title "record gh write goldens" --body-file "$work/body.md" --draft
	)
	number="$(gh pr list --repo "$scratch" --head "$branch" --state open --json number --jq '.[0].number')"
	record pr-list-found-scratch pr list --repo "$scratch" --head "$branch" --state open --json number,url,isDraft --limit 1
	record pr-edit pr edit "$number" --repo "$scratch" --title "record gh write goldens (edited)"
	record pr-ready pr ready "$number" --repo "$scratch"
	record pr-ready-undo pr ready "$number" --repo "$scratch" --undo
	gh pr close "$number" --repo "$scratch" --delete-branch
	rm -rf "$work"
	echo "record-gh-goldens: scratch pull request #$number closed; $scratch is yours to delete"
fi

printf '%s\n' "$version" >"$out/VERSION"
echo "record-gh-goldens: $version"
