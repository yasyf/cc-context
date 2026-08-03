package vcs

// GitRef is a git revision spelled the way git plumbing must receive it: fully
// qualified, so refname resolution cannot pick a different object than the one
// meant. git resolves a short name through refs/tags and refs/heads before
// refs/remotes, so a local branch or tag literally named "origin/main" answers
// merge-base, rebase, and merge in place of the remote-tracking ref — git warns
// on stderr and still exits 0. Every plumbing helper takes GitRef, so a short
// name reaching one is a compile error rather than a silent wrong answer.
type GitRef string

// HeadRef is the working copy's current commit, the one short name git resolves
// unambiguously.
const HeadRef GitRef = "HEAD"

// RemoteBranchRef qualifies a remote-tracking branch as refs/remotes/<remote>/<name>.
func RemoteBranchRef(remote, name string) GitRef {
	return GitRef("refs/remotes/" + remote + "/" + name)
}

// LocalBranchRef qualifies a local branch as refs/heads/<name>.
func LocalBranchRef(name string) GitRef {
	return GitRef("refs/heads/" + name)
}

// UnsafeRef wraps a revision ccx did not construct — the diff source a user
// typed, which may legitimately be any revision expression git parses (HEAD~2,
// a sha, a range endpoint, a bookmark@remote) and so cannot be qualified. It is
// the single escape out of GitRef's guarantee and exists for that one caller;
// a name ccx builds from a branch or remote goes through RemoteBranchRef or
// LocalBranchRef instead. GitArgs still interposes --end-of-options ahead of
// every rev, so an unsafe ref cannot inject an option even though it can name
// an ambiguous ref.
func UnsafeRef(s string) GitRef {
	return GitRef(s)
}

// Trunk is a repository's default branch, carrying both spellings the callers
// need so neither is derived at a call site: Name for display, gh --base,
// branch identity, and jj revsets; Ref for git plumbing, and only that.
type Trunk struct {
	name   string
	remote string
	ref    GitRef
}

// Name is the bare branch name (e.g. "main") — the spelling gh, jj, and reports
// take.
func (t Trunk) Name() string { return t.name }

// Remote names the remote whose default branch this is.
func (t Trunk) Remote() string { return t.remote }

// Ref is the fully qualified remote-tracking ref — the only spelling git
// plumbing may receive.
func (t Trunk) Ref() GitRef { return t.ref }
