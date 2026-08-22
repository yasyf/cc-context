package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/vcs"
)

// branchNoOptDefVal is cobra's NoOptDefVal for a bare --new-branch (and its
// --create alias): "-" is not a legal git branch name, so it never collides
// with an explicit --new-branch=name.
const branchNoOptDefVal = "-"

// branchNameMax caps a derived branch name, which is truncated to it on a word
// boundary.
const branchNameMax = 60

// branchAction is what ship does with the position it resolved: append the
// commit to the branch that is already there, or start a new one for it.
type branchAction int

const (
	branchAppend branchAction = iota
	branchCreate
)

// branchPlan is ship's single answer to "where does this commit go", resolved
// once before any mutation and consumed by every lane. from is where the working
// copy sat when it was resolved, which a git-lane create switches back to when
// the commit never lands.
type branchPlan struct {
	action branchAction
	name   string
	from   string
	parent string
	trunk  string

	needsRestack bool
}

// resolveBranchPlan turns the caller's stated intent and the working copy's
// current position into one decision. It is a pure function over already-read
// state so the whole matrix is table-testable: current is the branch or nearest
// bookmark the working copy sits on ("" for a detached HEAD), trunk the
// resolved trunk ("" when the lane cannot name one, which reads as "not on
// trunk").
//
// Two rows need a repository probe and so live with the lane that can make it:
// a --branch naming an existing branch other than the current one (refused by
// the git-backed preflights, since ship does not check branches out), and
// several trunk candidates (refused where trunk is resolved, which is the only
// place that can list them).
func resolveBranchPlan(l lane, r vcs.Repo, o shipOpts, current, trunk string) (branchPlan, error) {
	plan := branchPlan{name: current, from: current, parent: o.parent, trunk: trunk}
	onTrunk := trunk != "" && current == trunk

	// A jj working copy always has somewhere to commit, bookmark or not; only a
	// git-backed lane can be sitting on nothing.
	if current == "" && gitBacked(l) {
		return branchPlan{}, errors.New("ship: detached HEAD — check out a branch before shipping")
	}

	// An amend forms no new commit, so no rule that exists to keep a new commit
	// off trunk applies to it.
	if o.amend {
		plan.action = branchAppend
		if o.branch != "" {
			plan.name = o.branch
		}
		return plan, nil
	}

	// --no-commit forms no commit either, and placing one that already exists
	// onto a new branch would be a history rewrite, not a push.
	if o.noCommit {
		plan.action = branchAppend
		if o.branch != "" {
			plan.name = o.branch
		}
		return plan, nil
	}

	if o.newBranch != "" {
		name, err := newBranchName(o)
		if err != nil {
			return branchPlan{}, err
		}
		plan.action, plan.name = branchCreate, name
		return plan, nil
	}

	if o.branch != "" {
		switch {
		case o.branch == trunk && l.gt:
			return branchPlan{}, fmt.Errorf("ship: the graphite lane cannot commit onto trunk %q — pass --new-branch to stack a branch instead", trunk)
		case o.branch == trunk && !o.allowTrunk && trunkNeedsBranch(r):
			return branchPlan{}, fmt.Errorf("ship: --branch %s names trunk of %s, which you do not own — pass --allow-trunk to advance it deliberately, or --new-branch to stack a branch instead", trunk, r.NameWithOwner)
		case o.branch == current:
			plan.action = branchAppend
		default:
			plan.action, plan.name = branchCreate, o.branch
		}
		return plan, nil
	}

	if o.appendOnly && onTrunk {
		return branchPlan{}, errors.New("ship: append would commit onto trunk — pass --new-branch")
	}

	// gt has no verb that commits onto trunk: gt modify answers "Cannot perform
	// this operation on the trunk branch", so the lane always stacks a branch
	// there, personal repository or not.
	if onTrunk && (l.gt || trunkNeedsBranch(r)) {
		name := deriveBranchName("", o.message)
		if name == "" {
			return branchPlan{}, fmt.Errorf("ship: cannot derive a branch name from %q — pass --new-branch=<name>", firstLine(o.message))
		}
		plan.action, plan.name = branchCreate, name
		return plan, nil
	}

	plan.action = branchAppend
	return plan, nil
}

// newBranchName resolves --new-branch's argument: an explicit name as given, a
// bare flag from the commit subject.
func newBranchName(o shipOpts) (string, error) {
	if o.newBranch != branchNoOptDefVal {
		return o.newBranch, nil
	}
	name := deriveBranchName("", o.message)
	if name == "" {
		return "", fmt.Errorf("ship: cannot derive a branch name from %q — pass --new-branch=<name>", firstLine(o.message))
	}
	return name, nil
}

// gitBacked reports whether the lane commits onto a git branch — the graphite
// lane and the plain git lane, but not jj, whose bookmarks move independently
// of any checkout. A colocated jj repository on the graphite lane is git-backed.
func gitBacked(l lane) bool {
	return l.gt || l.kind == vcs.Git
}

// trunkNeedsBranch reports whether a commit formed on trunk has to go onto a
// new branch instead, which is true only when GitHub positively says the
// repository belongs to someone other than the viewer: an org trunk rejects the
// commit through its protect-<trunk> hook and leaves it dangling. An
// unanswerable lookup keeps the direct-to-trunk flow, the same way the lane
// gate only ever demotes on a positive answer.
func trunkNeedsBranch(r vcs.Repo) bool {
	return r.Owner != "" && r.ViewerLogin != "" && !r.Personal()
}

// deriveBranchName slugifies subject into a git-legal branch name under prefix.
// It reads the subject before withSessionTrailer appends the Claude-Session-Id
// trailer, which is what keeps the trailer out of the branch name. An empty
// return means no legal name came out of the subject.
//
// TODO: graphite derives its own names under ~/.graphite_user_config's
// branchPrefix; ship takes the naming over, so prefix likely wants to come from
// there.
func deriveBranchName(prefix, subject string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(firstLine(subject)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if room := branchNameMax - len(prefix); len(slug) > room {
		slug = strings.Trim(truncateAtWord(slug, room), "-")
	}
	name := prefix + slug
	if !legalBranchName(name) {
		return ""
	}
	return name
}

// truncateAtWord cuts slug to at most n bytes on a dash boundary, falling back
// to a hard cut when the first word alone overruns.
func truncateAtWord(slug string, n int) string {
	if n <= 0 {
		return ""
	}
	cut := slug[:n]
	if i := strings.LastIndexByte(cut, '-'); i > 0 {
		return cut[:i]
	}
	return cut
}

// legalBranchName reports whether name is a form git accepts as a branch: the
// subset of git-check-ref-format a slug can still trip over through its prefix.
func legalBranchName(name string) bool {
	switch {
	case name == "":
		return false
	case strings.HasPrefix(name, "-"):
		return false
	case strings.Contains(name, ".."):
		return false
	case strings.HasSuffix(name, ".lock"):
		return false
	case strings.HasSuffix(name, "/") || strings.HasSuffix(name, "."):
		return false
	default:
		return true
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
