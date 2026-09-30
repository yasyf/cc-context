package cleanup

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	testID        = "0000000000000001-abcdef"
	testHead      = "0123456789abcdef0123456789abcdef01234567"
	testLegacyID  = "0123456789abcdef0123"
	testLegacyRef = "refs/cleanup-worktrees/20260928/" + testLegacyID
	testSource    = "/home/u/.cache/codex-worktree-trash-20260928/" + testLegacyID + "-feature"
)

var testCreated = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func validJob(layout Layout, id string, seq uint64) Job {
	return Job{
		Schema:      Schema,
		ID:          id,
		Seq:         seq,
		Phase:       PhasePrepared,
		Repo:        "/repo/.git",
		AdminDir:    "/repo/.git/worktrees/feature",
		Admin:       FileID{Dev: 1, Ino: 2},
		Original:    "/work/feature",
		Registered:  layout.Registered(id),
		Payload:     layout.Payload(id),
		Tree:        FileID{Dev: 1, Ino: 3},
		Head:        testHead,
		Branch:      "feature",
		RecoveryRef: RecoveryRefFor(id),
		Git:         "/usr/bin/git",
		Links:       Links{DotGit: "gitdir: /repo/.git/worktrees/feature\n", AdminGitdir: "/work/feature/.git\n"},
		Created:     testCreated,
		Updated:     testCreated,
	}
}

func adoptedJob(layout Layout, id string, seq uint64) Job {
	return Job{
		Schema:      Schema,
		ID:          id,
		Seq:         seq,
		Phase:       PhasePrepared,
		Adopted:     true,
		Source:      testSource,
		Owner:       "legacy quarantine import",
		Repo:        "/repo/.git",
		Original:    "/work/feature",
		Registered:  layout.Registered(id),
		Payload:     layout.Payload(id),
		Tree:        FileID{Dev: 1, Ino: 4},
		Head:        testHead,
		RecoveryRef: testLegacyRef,
		Git:         "/usr/bin/git",
		Created:     testCreated,
		Updated:     testCreated,
	}
}

func TestJobValidate(t *testing.T) {
	layout := Layout{Root: "/state"}
	tests := []struct {
		name   string
		mutate func(*Job)
		want   string
	}{
		{"valid", func(*Job) {}, ""},
		{"valid deferred queued", func(j *Job) { j.Deferred, j.Phase = true, PhaseQueued }, ""},
		{"valid deferred waiting", func(j *Job) { j.Deferred, j.Phase = true, PhaseWaiting }, ""},
		{"valid unborn head", func(j *Job) { j.Head, j.RecoveryRef = "", "" }, ""},
		{"valid sha256 head", func(j *Job) { j.Head = strings.Repeat("ab", 32) }, ""},
		{"valid blocked", func(j *Job) { j.Block(testCreated, "git", "move failed") }, ""},
		{"valid full error history", func(j *Job) { j.Errors = make([]JobError, MaxJobErrors) }, ""},
		{"schema", func(j *Job) { j.Schema = 2 }, "schema 2, want 1"},
		{"id", func(j *Job) { j.ID = "0000000000000001-ABCDEF" }, `id "0000000000000001-ABCDEF" is not a job id`},
		{"seq zero", func(j *Job) { j.Seq = 0 }, "seq is zero"},
		{"unknown phase", func(j *Job) { j.Phase = "bogus" }, `unknown phase "bogus"`},
		{"queued not deferred", func(j *Job) { j.Phase = PhaseQueued }, `phase "queued" belongs to a deferred job`},
		{"waiting not deferred", func(j *Job) { j.Phase = PhaseWaiting }, `phase "waiting" belongs to a deferred job`},
		{"relative repo", func(j *Job) { j.Repo = "repo/.git" }, `repo "repo/.git" is not a clean absolute path`},
		{"unclean admin_dir", func(j *Job) { j.AdminDir = "/repo/.git/worktrees/x/../feature" }, `admin_dir "/repo/.git/worktrees/x/../feature" is not a clean absolute path`},
		{"relative original", func(j *Job) { j.Original = "feature" }, `original "feature" is not a clean absolute path`},
		{"unclean registered", func(j *Job) { j.Registered += "/" }, `registered "/state/jobs/` + testID + `/registered/" is not a clean absolute path`},
		{"empty payload", func(j *Job) { j.Payload = "" }, `payload "" is not a clean absolute path`},
		{"relative git", func(j *Job) { j.Git = "git" }, `git "git" is not a clean absolute path`},
		{"admin_dir outside worktrees", func(j *Job) { j.AdminDir = "/repo/.git/modules/feature" }, `admin_dir "/repo/.git/modules/feature" is not a worktree admin directory of "/repo/.git"`},
		{"admin_dir of another repo", func(j *Job) { j.AdminDir = "/other/.git/worktrees/feature" }, `admin_dir "/other/.git/worktrees/feature" is not a worktree admin directory of "/repo/.git"`},
		{"registered in another job's folder", func(j *Job) { j.Registered = layout.Registered("0000000000000002-abcdef") }, `registered "/state/jobs/0000000000000002-abcdef/registered" is not job ` + testID + `'s private path`},
		{"registered misnamed", func(j *Job) { j.Registered = layout.JobDir(testID) + "/tree" }, `registered "/state/jobs/` + testID + `/tree" is not job ` + testID + `'s private path`},
		{"payload outside job folder", func(j *Job) { j.Payload = "/state/jobs/payload" }, `payload "/state/jobs/payload" is not job ` + testID + `'s private path`},
		{"tree identity zero", func(j *Job) { j.Tree = FileID{} }, "tree identity was never captured"},
		{"admin identity zero", func(j *Job) { j.Admin = FileID{} }, "tree or admin identity was never captured"},
		{"head not an oid", func(j *Job) { j.Head = "HEAD" }, `head "HEAD" is not an object id`},
		{"head short", func(j *Job) { j.Head = testHead[:39] }, `head "` + testHead[:39] + `" is not an object id`},
		{"recovery ref without head", func(j *Job) { j.Head = "" }, `recovery_ref "refs/ccx/cleanup/` + testID + `" set with no head to pin`},
		{"head without recovery ref", func(j *Job) { j.RecoveryRef = "" }, `recovery_ref "", want "refs/ccx/cleanup/` + testID + `"`},
		{"recovery ref of another job", func(j *Job) { j.RecoveryRef = RecoveryRefFor("0000000000000002-abcdef") }, `recovery_ref "refs/ccx/cleanup/0000000000000002-abcdef", want "refs/ccx/cleanup/` + testID + `"`},
		{"dot git uncaptured", func(j *Job) { j.Links.DotGit = "" }, "link contents were never captured"},
		{"admin gitdir uncaptured", func(j *Job) { j.Links.AdminGitdir = "" }, "link contents were never captured"},
		{"blockage without reason", func(j *Job) { j.Blocked = &Blockage{Detail: "d", At: testCreated} }, "blockage carries no reason or detail"},
		{"blockage without detail", func(j *Job) { j.Blocked = &Blockage{Reason: "git", At: testCreated} }, "blockage carries no reason or detail"},
		{"error history over bound", func(j *Job) { j.Errors = make([]JobError, MaxJobErrors+1) }, "9 errors, at most 8"},
		{"created unset", func(j *Job) { j.Created = time.Time{} }, "created or updated is unset"},
		{"updated unset", func(j *Job) { j.Updated = time.Time{} }, "created or updated is unset"},
		{"source on a linked job", func(j *Job) { j.Source = testSource }, `source "` + testSource + `" set on a job that adopted nothing`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob(layout, testID, 1)
			tt.mutate(&job)
			err := job.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestJobState(t *testing.T) {
	blocked := &Blockage{Reason: "git", Detail: "move failed", At: testCreated}
	tests := []struct {
		name    string
		phase   Phase
		blocked *Blockage
		want    State
	}{
		{"waiting", PhaseWaiting, nil, State("waiting")},
		{"running", PhaseMoved, nil, State("moved")},
		{"deleting", PhaseDeleting, nil, State("deleting")},
		{"done", PhaseDone, nil, State("done")},
		{"blocked", PhaseDetached, blocked, StateBlocked},
		{"blocked deleting", PhaseDeleting, blocked, StateBlocked},
		{"blocked done", PhaseDone, blocked, StateBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := Job{Phase: tt.phase, Blocked: tt.blocked}
			if got := job.State(); got != tt.want {
				t.Errorf("State() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobValidateAdopted(t *testing.T) {
	layout := Layout{Root: "/state"}
	tests := []struct {
		name   string
		mutate func(*Job)
		want   string
	}{
		{"valid", func(*Job) {}, ""},
		{"valid unregistered", func(j *Job) { j.Phase = PhaseUnregistered }, ""},
		{"valid deleting", func(j *Job) { j.Phase, j.Removed = PhaseDeleting, 40 }, ""},
		{"valid done", func(j *Job) { j.Phase = PhaseDone }, ""},
		{"valid blocked", func(j *Job) { j.Block(testCreated, "reconcile", "source replaced") }, ""},
		{"valid sha256 head", func(j *Job) { j.Head = strings.Repeat("cd", 32) }, ""},
		{"deferred", func(j *Job) { j.Deferred = true }, "an adopted job cannot be deferred"},
		{"deferred queued", func(j *Job) { j.Deferred, j.Phase = true, PhaseQueued }, "an adopted job cannot be deferred"},
		{"queued", func(j *Job) { j.Phase = PhaseQueued }, `phase "queued" belongs to a deferred job`},
		{"waiting", func(j *Job) { j.Phase = PhaseWaiting }, `phase "waiting" belongs to a deferred job`},
		{"moved", func(j *Job) { j.Phase = PhaseMoved }, `phase "moved" belongs to a registered worktree, not an adopted tree`},
		{"detached", func(j *Job) { j.Phase = PhaseDetached }, `phase "detached" belongs to a registered worktree, not an adopted tree`},
		{"empty source", func(j *Job) { j.Source = "" }, `source "" is not a clean absolute path`},
		{"relative source", func(j *Job) { j.Source = "codex-worktree-trash-20260928/" + testLegacyID + "-feature" }, `source "codex-worktree-trash-20260928/` + testLegacyID + `-feature" is not a clean absolute path`},
		{"unclean source", func(j *Job) { j.Source += "/" }, `source "` + testSource + `/" is not a clean absolute path`},
		{"admin_dir", func(j *Job) { j.AdminDir = "/repo/.git/worktrees/feature" }, "an adopted job carries admin or link state it never had"},
		{"admin identity", func(j *Job) { j.Admin = FileID{Dev: 1, Ino: 2} }, "an adopted job carries admin or link state it never had"},
		{"dot git link", func(j *Job) { j.Links.DotGit = "gitdir: /repo/.git/worktrees/feature\n" }, "an adopted job carries admin or link state it never had"},
		{"admin gitdir link", func(j *Job) { j.Links.AdminGitdir = testSource + "/.git\n" }, "an adopted job carries admin or link state it never had"},
		{"no head", func(j *Job) { j.Head = "" }, "an adopted job carries no head"},
		{"head not an oid", func(j *Job) { j.Head = "HEAD" }, `head "HEAD" is not an object id`},
		{"tree identity zero", func(j *Job) { j.Tree = FileID{} }, "tree identity was never captured"},
		{"recovery ref of a job", func(j *Job) { j.RecoveryRef = RecoveryRefFor(testID) }, `recovery_ref "refs/ccx/cleanup/` + testID + `" is not refs/cleanup-worktrees/<date>/<id>`},
		{"no recovery ref", func(j *Job) { j.RecoveryRef = "" }, `recovery_ref "" is not refs/cleanup-worktrees/<date>/<id>`},
		{"source outside the ref's quarantine", func(j *Job) { j.Source = "/home/u/.cache/codex-worktree-trash-20260929/" + testLegacyID + "-feature" }, `source "/home/u/.cache/codex-worktree-trash-20260929/` + testLegacyID + `-feature" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"source of another id", func(j *Job) { j.Source = "/home/u/.cache/codex-worktree-trash-20260928/ffffffffffffffffffff-feature" }, `source "/home/u/.cache/codex-worktree-trash-20260928/ffffffffffffffffffff-feature" is not the tree ` + testLegacyRef + ` pins`},
		{"source with no name", func(j *Job) { j.Source = "/home/u/.cache/codex-worktree-trash-20260928/" + testLegacyID + "-" }, `source "/home/u/.cache/codex-worktree-trash-20260928/` + testLegacyID + `-" is not the tree ` + testLegacyRef + ` pins`},
		{"registered in another job's folder", func(j *Job) { j.Registered = layout.Registered("0000000000000002-abcdef") }, `registered "/state/jobs/0000000000000002-abcdef/registered" is not job ` + testID + `'s private path`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := adoptedJob(layout, testID, 1)
			tt.mutate(&job)
			err := job.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestJobNoteBoundsHistory(t *testing.T) {
	job := Job{Phase: PhaseDeleting}
	for i := range MaxJobErrors + 3 {
		job.Note(testCreated.Add(time.Duration(i)*time.Second), fmt.Sprintf("m%d", i))
	}
	if len(job.Errors) != MaxJobErrors {
		t.Fatalf("len(Errors) = %d, want %d", len(job.Errors), MaxJobErrors)
	}
	for i, e := range job.Errors {
		n := i + 3
		want := JobError{At: testCreated.Add(time.Duration(n) * time.Second), Phase: PhaseDeleting, Message: fmt.Sprintf("m%d", n)}
		if e != want {
			t.Errorf("Errors[%d] = %+v, want %+v", i, e, want)
		}
	}
}

func TestJobNoteRecordsCurrentPhase(t *testing.T) {
	job := Job{Phase: PhasePrepared}
	job.Note(testCreated, "first")
	job.Phase = PhaseMoved
	job.Note(testCreated, "second")
	want := []JobError{
		{At: testCreated, Phase: PhasePrepared, Message: "first"},
		{At: testCreated, Phase: PhaseMoved, Message: "second"},
	}
	if len(job.Errors) != len(want) || job.Errors[0] != want[0] || job.Errors[1] != want[1] {
		t.Errorf("Errors = %+v, want %+v", job.Errors, want)
	}
}

func TestJobBlock(t *testing.T) {
	job := validJob(Layout{Root: "/state"}, testID, 1)
	job.Phase = PhaseMoved
	at := testCreated.Add(time.Minute)
	job.Block(at, "identity", "tree was replaced")

	if want := (Blockage{Reason: "identity", Detail: "tree was replaced", At: at}); job.Blocked == nil || *job.Blocked != want {
		t.Fatalf("Blocked = %+v, want %+v", job.Blocked, want)
	}
	if want := []JobError{{At: at, Phase: PhaseMoved, Message: "identity: tree was replaced"}}; len(job.Errors) != 1 || job.Errors[0] != want[0] {
		t.Errorf("Errors = %+v, want %+v", job.Errors, want)
	}
	if !job.Updated.Equal(at) {
		t.Errorf("Updated = %v, want %v", job.Updated, at)
	}
	if got := job.State(); got != StateBlocked {
		t.Errorf("State() = %q, want %q", got, StateBlocked)
	}
	if err := job.Validate(); err != nil {
		t.Errorf("Validate() after Block = %v, want nil", err)
	}
}

func TestMintIDFormat(t *testing.T) {
	now := time.Unix(0, 0x18a2b3c4d5e6f708)
	id := MintID(now)
	if !regexp.MustCompile(`^18a2b3c4d5e6f708-[0-9a-f]{6}$`).MatchString(id) {
		t.Fatalf("MintID() = %q, want 18a2b3c4d5e6f708-<6 hex>", id)
	}
	if other := MintID(now); other == id {
		t.Errorf("MintID() twice at one instant = %q both times, want distinct suffixes", id)
	}
	job := validJob(Layout{Root: "/state"}, id, 1)
	if err := job.Validate(); err != nil {
		t.Errorf("Validate() of a minted id = %v, want nil", err)
	}
}

func TestMintIDOrdersByCreation(t *testing.T) {
	instants := []time.Time{
		time.Unix(0, 0xf),
		time.Unix(0, 0x10),
		time.Unix(0, 0xff),
		time.Unix(0, 0x100),
		time.Unix(1, 0),
		testCreated,
		testCreated.Add(time.Nanosecond),
	}
	ids := make([]string, len(instants))
	for i, at := range instants {
		ids[i] = MintID(at)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if sorted[i] != ids[i] {
			t.Fatalf("sorted ids = %v, want creation order %v", sorted, ids)
		}
	}
}

func TestPhaseHalves(t *testing.T) {
	tests := []struct {
		phase    Phase
		logical  bool
		physical bool
	}{
		{PhaseQueued, true, false},
		{PhaseWaiting, true, false},
		{PhasePrepared, true, false},
		{PhaseMoved, true, false},
		{PhaseDetached, true, false},
		{PhaseUnregistered, false, true},
		{PhaseDeleting, false, true},
		{PhaseDone, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			if got := tt.phase.Logical(); got != tt.logical {
				t.Errorf("Logical() = %v, want %v", got, tt.logical)
			}
			if got := tt.phase.Physical(); got != tt.physical {
				t.Errorf("Physical() = %v, want %v", got, tt.physical)
			}
		})
	}
	if len(tests) != len(phaseRank) {
		t.Errorf("table covers %d phases, phaseRank has %d", len(tests), len(phaseRank))
	}
}
