package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
)

// personalRepo and orgRepo are the two answers Repo.Personal gives; unknownRepo
// is the third state, a lookup that could not be made at all.
var (
	personalRepo = vcs.Repo{NameWithOwner: "yasyf/cc-context", Owner: "yasyf", ViewerLogin: "yasyf"}
	orgRepo      = vcs.Repo{NameWithOwner: "anthropics/claude-code", Owner: "anthropics", ViewerLogin: "yasyf"}
	unknownRepo  = vcs.Repo{}
)

var (
	jjLane  = lane{kind: vcs.JJ}
	gitLane = lane{kind: vcs.Git}
	gtLane  = lane{kind: vcs.Git, gt: true}
)

func TestResolveBranchPlan(t *testing.T) {
	msg := "fix: frobnicate the widget"
	tests := []struct {
		name    string
		lane    lane
		repo    vcs.Repo
		o       shipOpts
		current string
		trunk   string
		want    branchPlan
		wantErr string
	}{
		{
			name: "on trunk in your own repo appends", lane: gitLane, repo: personalRepo,
			o: shipOpts{message: msg}, current: "main", trunk: "main",
			want: branchPlan{action: branchAppend, name: "main", from: "main", trunk: "main"},
		},
		{
			name: "on trunk in an org repo creates", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg}, current: "main", trunk: "main",
			want: branchPlan{action: branchCreate, name: "fix-frobnicate-the-widget", from: "main", trunk: "main"},
		},
		{
			name: "an unanswerable lookup keeps the direct-to-trunk flow", lane: gitLane, repo: unknownRepo,
			o: shipOpts{message: msg}, current: "main", trunk: "main",
			want: branchPlan{action: branchAppend, name: "main", from: "main", trunk: "main"},
		},
		{
			name: "the graphite lane always stacks a branch on trunk", lane: gtLane, repo: personalRepo,
			o: shipOpts{message: msg}, current: "main", trunk: "main",
			want: branchPlan{action: branchCreate, name: "fix-frobnicate-the-widget", from: "main", trunk: "main"},
		},
		{
			name: "on a non-trunk branch appends to it", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg}, current: "feature", trunk: "main",
			want: branchPlan{action: branchAppend, name: "feature", from: "feature", trunk: "main"},
		},
		{
			name: "a jj bookmark that is not trunk appends, never refuses", lane: jjLane, repo: orgRepo,
			o: shipOpts{message: msg}, current: "someone/probe", trunk: "main",
			want: branchPlan{action: branchAppend, name: "someone/probe", from: "someone/probe", trunk: "main"},
		},
		{
			name: "an unresolvable trunk reads as not on trunk", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg}, current: "main", trunk: "",
			want: branchPlan{action: branchAppend, name: "main", from: "main"},
		},
		{
			name: "detached HEAD refuses", lane: gitLane, repo: personalRepo,
			o: shipOpts{message: msg}, current: "", trunk: "main",
			wantErr: "ship: detached HEAD — check out a branch before shipping",
		},
		{
			name: "a jj repo with no bookmark at all is not detached", lane: jjLane, repo: personalRepo,
			o: shipOpts{message: msg}, current: "", trunk: "",
			want: branchPlan{action: branchAppend},
		},
		{
			name: "--branch naming trunk of an org repo refuses", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg, branch: "main"}, current: "feature", trunk: "main",
			wantErr: "ship: --branch main names trunk of anthropics/claude-code, which you do not own — pass --allow-trunk to advance it deliberately, or --new-branch to stack a branch instead",
		},
		{
			name: "--branch naming trunk of an org repo with --allow-trunk creates it here", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg, branch: "main", allowTrunk: true}, current: "feature", trunk: "main",
			want: branchPlan{action: branchCreate, name: "main", from: "feature", trunk: "main"},
		},
		{
			name: "--branch naming trunk of your own repo appends", lane: jjLane, repo: personalRepo,
			o: shipOpts{message: msg, branch: "main"}, current: "main", trunk: "main",
			want: branchPlan{action: branchAppend, name: "main", from: "main", trunk: "main"},
		},
		{
			name: "--branch naming trunk refuses on the graphite lane", lane: gtLane, repo: personalRepo,
			o: shipOpts{message: msg, branch: "main", allowTrunk: true}, current: "feature", trunk: "main",
			wantErr: `ship: the graphite lane cannot commit onto trunk "main" — pass --new-branch to stack a branch instead`,
		},
		{
			name: "--branch naming the current branch appends", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg, branch: "feature"}, current: "feature", trunk: "main",
			want: branchPlan{action: branchAppend, name: "feature", from: "feature", trunk: "main"},
		},
		{
			name: "--branch naming another branch creates it here", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg, branch: "other"}, current: "feature", trunk: "main",
			want: branchPlan{action: branchCreate, name: "other", from: "feature", trunk: "main"},
		},
		{
			name: "--append on trunk refuses", lane: gitLane, repo: personalRepo,
			o: shipOpts{message: msg, appendOnly: true}, current: "main", trunk: "main",
			wantErr: "ship: append would commit onto trunk — pass --new-branch",
		},
		{
			name: "--append off trunk appends", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: msg, appendOnly: true}, current: "feature", trunk: "main",
			want: branchPlan{action: branchAppend, name: "feature", from: "feature", trunk: "main"},
		},
		{
			name: "bare --new-branch derives the name", lane: gitLane, repo: personalRepo,
			o: shipOpts{message: msg, newBranch: branchNoOptDefVal}, current: "main", trunk: "main",
			want: branchPlan{action: branchCreate, name: "fix-frobnicate-the-widget", from: "main", trunk: "main"},
		},
		{
			name: "--new-branch=name uses it verbatim", lane: gitLane, repo: personalRepo,
			o: shipOpts{message: msg, newBranch: "explicit/name"}, current: "feature", trunk: "main",
			want: branchPlan{action: branchCreate, name: "explicit/name", from: "feature", trunk: "main"},
		},
		{
			name: "bare --new-branch on a message with no slug refuses", lane: gitLane, repo: personalRepo,
			o: shipOpts{message: "!!!", newBranch: branchNoOptDefVal}, current: "feature", trunk: "main",
			wantErr: `ship: cannot derive a branch name from "!!!" — pass --new-branch=<name>`,
		},
		{
			name: "an org trunk whose subject yields no slug refuses", lane: gitLane, repo: orgRepo,
			o: shipOpts{message: "???"}, current: "main", trunk: "main",
			wantErr: `ship: cannot derive a branch name from "???" — pass --new-branch=<name>`,
		},
		{
			name: "--amend on an org trunk still appends", lane: gitLane, repo: orgRepo,
			o: shipOpts{amend: true}, current: "main", trunk: "main",
			want: branchPlan{action: branchAppend, name: "main", from: "main", trunk: "main"},
		},
		{
			name: "--amend keeps a --branch target", lane: jjLane, repo: orgRepo,
			o: shipOpts{amend: true, branch: "someone/probe"}, current: "main", trunk: "main",
			want: branchPlan{action: branchAppend, name: "someone/probe", from: "main", trunk: "main"},
		},
		{
			name: "--parent rides along on a create", lane: gtLane, repo: orgRepo,
			o: shipOpts{message: msg, newBranch: "stacked", parent: "base"}, current: "feature", trunk: "main",
			want: branchPlan{action: branchCreate, name: "stacked", from: "feature", parent: "base", trunk: "main"},
		},
		{
			name: "the derived name reads the subject, not the session trailer", lane: gtLane, repo: personalRepo,
			o: shipOpts{message: msg + "\n\nClaude-Session-Id: 0e1d2c3b"}, current: "main", trunk: "main",
			want: branchPlan{action: branchCreate, name: "fix-frobnicate-the-widget", from: "main", trunk: "main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBranchPlan(tt.lane, tt.repo, tt.o, tt.current, tt.trunk)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBranchPlan error = %v", err)
			}
			if got != tt.want {
				t.Errorf("plan = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDeriveBranchName(t *testing.T) {
	long := "feat: add a thoroughly overengineered configuration subsystem for every lane"
	tests := []struct {
		name    string
		prefix  string
		subject string
		want    string
	}{
		{name: "slugifies and lowercases", subject: "Fix: Frobnicate The Widget!", want: "fix-frobnicate-the-widget"},
		{name: "collapses runs of non-alphanumerics", subject: "fix -- (a)  //b//", want: "fix-a-b"},
		{name: "keeps digits", subject: "bump v2 to 3", want: "bump-v2-to-3"},
		{name: "reads only the subject line", subject: "fix: widget\n\nbody text here", want: "fix-widget"},
		{
			name:    "drops the session trailer with the rest of the body",
			subject: "fix: widget\n\nClaude-Session-Id: 4f0e-1d2c-3b4a",
			want:    "fix-widget",
		},
		{name: "truncates at 60 on a word boundary", subject: long, want: "feat-add-a-thoroughly-overengineered-configuration"},
		{name: "hard-cuts a single overlong word", subject: strings.Repeat("x", 70), want: strings.Repeat("x", 60)},
		{name: "applies the prefix", prefix: "yasyf/", subject: "fix: widget", want: "yasyf/fix-widget"},
		{name: "truncates inside the prefix's budget", prefix: "yasyf/", subject: long, want: "yasyf/feat-add-a-thoroughly-overengineered-configuration"},
		{name: "no alphanumerics yields nothing", subject: "!!! ???", want: ""},
		{name: "empty subject yields nothing", subject: "", want: ""},
		{name: "rejects a leading dash", prefix: "-", subject: "fix widget", want: ""},
		{name: "rejects a doubled dot", prefix: "a..b/", subject: "fix widget", want: ""},
		{name: "rejects a trailing slash", prefix: "trailing/", subject: "!!!", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveBranchName(tt.prefix, tt.subject); got != tt.want {
				t.Errorf("deriveBranchName(%q, %q) = %q, want %q", tt.prefix, tt.subject, got, tt.want)
			}
		})
	}
}

// TestDeriveBranchNameLegality guards the forms git-check-ref-format rejects
// that a prefix can still smuggle past slugification.
func TestDeriveBranchNameLegality(t *testing.T) {
	for _, name := range []string{"", "-lead", "a..b", "feature.lock", "trailing/", "trailing."} {
		if legalBranchName(name) {
			t.Errorf("legalBranchName(%q) = true, want false", name)
		}
	}
	for _, name := range []string{"fix-widget", "yasyf/fix-widget", "v2.1-bump", "a"} {
		if !legalBranchName(name) {
			t.Errorf("legalBranchName(%q) = false, want true", name)
		}
	}
}
