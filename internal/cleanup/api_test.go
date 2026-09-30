package cleanup

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestActiveErrorMessage(t *testing.T) {
	tests := []struct {
		name    string
		holders []Holder
		want    string
	}{
		{
			"cwd",
			[]Holder{{PID: 42, Name: "vim", Evidence: EvidenceCwd, Path: "/w/src"}},
			"/w is in use: vim (pid 42) working in /w/src",
		},
		{
			"fd",
			[]Holder{{PID: 7, Name: "sleep", Evidence: EvidenceFD, Path: "/w/file"}},
			"/w is in use: sleep (pid 7) holding open /w/file",
		},
		{
			"argv",
			[]Holder{{PID: 9, Name: "sh", Evidence: EvidenceArgv, Path: "--root=/w/sub"}},
			"/w is in use: sh (pid 9) started with --root=/w/sub",
		},
		{
			"on a terminal",
			[]Holder{{PID: 42, Name: "zsh", TTY: true, Evidence: EvidenceCwd, Path: "/w"}},
			"/w is in use: zsh (pid 42 on a terminal) working in /w",
		},
		{
			"every kind in order",
			[]Holder{
				{PID: 1, Name: "a", Evidence: EvidenceCwd, Path: "/w"},
				{PID: 2, Name: "b", TTY: true, Evidence: EvidenceFD, Path: "/w/f"},
				{PID: 3, Name: "c", Evidence: EvidenceArgv, Path: "/w/g"},
			},
			"/w is in use: a (pid 1) working in /w; b (pid 2 on a terminal) holding open /w/f; c (pid 3) started with /w/g",
		},
		{
			"five shown in full",
			holders(5),
			"/w is in use: p1 (pid 1) working in /w; p2 (pid 2) working in /w; p3 (pid 3) working in /w; p4 (pid 4) working in /w; p5 (pid 5) working in /w",
		},
		{
			"past five summarized",
			holders(8),
			"/w is in use: p1 (pid 1) working in /w; p2 (pid 2) working in /w; p3 (pid 3) working in /w; p4 (pid 4) working in /w; p5 (pid 5) working in /w; and 3 more",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &ActiveError{Worktree: "/w", Holders: tt.holders}
			if got := err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func holders(n int) []Holder {
	out := make([]Holder, n)
	for i := range out {
		out[i] = Holder{PID: i + 1, Name: "p" + string(rune('1'+i)), Evidence: EvidenceCwd, Path: "/w"}
	}
	return out
}

func TestHolderJSON(t *testing.T) {
	tests := []struct {
		name   string
		holder Holder
		want   string
	}{
		{"terminal", Holder{PID: 42, Name: "vim", TTY: true, Evidence: EvidenceFD, Path: "/w/f"}, `{"pid":42,"name":"vim","tty":true,"evidence":"fd","path":"/w/f"}`},
		{"no terminal", Holder{PID: 7, Name: "sh", Evidence: EvidenceArgv, Path: "/w"}, `{"pid":7,"name":"sh","evidence":"argv","path":"/w"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.holder)
			if err != nil || string(data) != tt.want {
				t.Fatalf("Marshal() = %s, %v; want %s", data, err, tt.want)
			}
			var back Holder
			if err := json.Unmarshal(data, &back); err != nil || back != tt.holder {
				t.Errorf("Unmarshal() = %+v, %v; want %+v", back, err, tt.holder)
			}
		})
	}
}

func TestEvidenceKinds(t *testing.T) {
	if EvidenceCwd != "cwd" || EvidenceFD != "fd" || EvidenceArgv != "argv" {
		t.Errorf("evidence kinds = %q, %q, %q; want cwd, fd, argv", EvidenceCwd, EvidenceFD, EvidenceArgv)
	}
}

func TestRefusedErrorMessage(t *testing.T) {
	err := &RefusedError{Worktree: "/w", Reason: "dirty", Detail: "uncommitted changes: a.txt"}
	if got, want := err.Error(), "/w: uncommitted changes: a.txt"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestBlockedErrorMessage(t *testing.T) {
	job := validJob(Layout{Root: "/state"}, testID, 1)
	job.Phase = PhaseMoved
	job.Block(testCreated, "activity", "vim holds the tree")
	err := &BlockedError{Job: job}
	if got, want := err.Error(), "cleanup job "+testID+" is blocked at moved (activity): vim holds the tree"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestReceiptOf(t *testing.T) {
	job := validJob(Layout{Root: "/state"}, testID, 1)
	job.Phase = PhaseUnregistered
	want := Receipt{JobID: testID, State: State("unregistered"), Original: "/work/feature", RecoveryRef: RecoveryRefFor(testID)}
	if got := ReceiptOf(job); got != want {
		t.Errorf("ReceiptOf() = %+v, want %+v", got, want)
	}
	job.Block(testCreated, "git", "remove failed")
	want.State = StateBlocked
	if got := ReceiptOf(job); got != want {
		t.Errorf("ReceiptOf(blocked) = %+v, want %+v", got, want)
	}
}

func TestRequestValidate(t *testing.T) {
	valid := Request{Worktree: "/work/feature", Git: "/usr/bin/git"}
	tests := []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{"valid", func(*Request) {}, ""},
		{"valid force", func(r *Request) { r.Force = true }, ""},
		{"relative worktree", func(r *Request) { r.Worktree = "feature" }, `worktree "feature" is not a clean absolute path`},
		{"unclean worktree", func(r *Request) { r.Worktree = "/work/../work/feature" }, `worktree "/work/../work/feature" is not a clean absolute path`},
		{"empty git", func(r *Request) { r.Git = "" }, `git "" is not a clean absolute path`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)
			assertValidate(t, r.Validate(), tt.want)
		})
	}
}

func TestDeferRequestValidate(t *testing.T) {
	valid := DeferRequest{Worktree: "/work/feature", CommonDir: "/repo/.git", Owner: "stack", Git: "/usr/bin/git"}
	tests := []struct {
		name   string
		mutate func(*DeferRequest)
		want   string
	}{
		{"valid", func(*DeferRequest) {}, ""},
		{"empty owner", func(r *DeferRequest) { r.Owner = "" }, "owner is empty"},
		{"blank owner", func(r *DeferRequest) { r.Owner = " \t" }, "owner is empty"},
		{"relative worktree", func(r *DeferRequest) { r.Worktree = "feature" }, `worktree "feature" is not a clean absolute path`},
		{"unclean common dir", func(r *DeferRequest) { r.CommonDir = "/repo/.git/" }, `common_dir "/repo/.git/" is not a clean absolute path`},
		{"relative git", func(r *DeferRequest) { r.Git = "git" }, `git "git" is not a clean absolute path`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)
			assertValidate(t, r.Validate(), tt.want)
		})
	}
}

func validAdoptRequest() AdoptRequest {
	return AdoptRequest{
		Source:      testSource,
		Tree:        FileID{Dev: 1, Ino: 4},
		CommonDir:   "/repo/.git",
		Head:        testHead,
		RecoveryRef: testLegacyRef,
		Original:    "/work/feature",
		Owner:       "legacy quarantine import",
		Git:         "/usr/bin/git",
	}
}

func TestAdoptRequestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AdoptRequest)
		want   string
	}{
		{"valid", func(*AdoptRequest) {}, ""},
		{"valid sha256 head", func(r *AdoptRequest) { r.Head = strings.Repeat("ef", 32) }, ""},
		{"empty owner", func(r *AdoptRequest) { r.Owner = "" }, "owner is empty"},
		{"blank owner", func(r *AdoptRequest) { r.Owner = "  " }, "owner is empty"},
		{"tree unset", func(r *AdoptRequest) { r.Tree = FileID{} }, "tree identity is unset"},
		{"empty head", func(r *AdoptRequest) { r.Head = "" }, `head "" is not an object id`},
		{"symbolic head", func(r *AdoptRequest) { r.Head = "HEAD" }, `head "HEAD" is not an object id`},
		{"uppercase head", func(r *AdoptRequest) { r.Head = strings.ToUpper(testHead) }, `head "` + strings.ToUpper(testHead) + `" is not an object id`},
		{"relative source", func(r *AdoptRequest) { r.Source = "codex-worktree-trash-20260928/" + testLegacyID + "-feature" }, `source "codex-worktree-trash-20260928/` + testLegacyID + `-feature" is not a clean absolute path`},
		{"unclean source", func(r *AdoptRequest) {
			r.Source = "/home/u/.cache/x/../codex-worktree-trash-20260928/" + testLegacyID + "-feature"
		}, `source "/home/u/.cache/x/../codex-worktree-trash-20260928/` + testLegacyID + `-feature" is not a clean absolute path`},
		{"relative common dir", func(r *AdoptRequest) { r.CommonDir = ".git" }, `common_dir ".git" is not a clean absolute path`},
		{"empty original", func(r *AdoptRequest) { r.Original = "" }, `original "" is not a clean absolute path`},
		{"relative git", func(r *AdoptRequest) { r.Git = "git" }, `git "git" is not a clean absolute path`},
		{"ref of another namespace", func(r *AdoptRequest) { r.RecoveryRef = RecoveryRefFor(testID) }, `recovery_ref "refs/ccx/cleanup/` + testID + `" is not refs/cleanup-worktrees/<date>/<id>`},
		{"source outside the ref's quarantine", func(r *AdoptRequest) { r.Source = "/home/u/.cache/trash/" + testLegacyID + "-feature" }, `source "/home/u/.cache/trash/` + testLegacyID + `-feature" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"source of another id", func(r *AdoptRequest) { r.Source = "/home/u/.cache/codex-worktree-trash-20260928/feature" }, `source "/home/u/.cache/codex-worktree-trash-20260928/feature" is not the tree ` + testLegacyRef + ` pins`},
		{"source with no name", func(r *AdoptRequest) { r.Source = "/home/u/.cache/codex-worktree-trash-20260928/" + testLegacyID + "-" }, `source "/home/u/.cache/codex-worktree-trash-20260928/` + testLegacyID + `-" is not the tree ` + testLegacyRef + ` pins`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validAdoptRequest()
			tt.mutate(&r)
			assertValidate(t, r.Validate(), tt.want)
		})
	}
}

func TestLegacyBinding(t *testing.T) {
	const (
		quarantine = "/home/u/.cache/codex-worktree-trash-20260928"
		source     = quarantine + "/" + testLegacyID + "-feature"
		refShape   = " is not refs/cleanup-worktrees/<date>/<id>"
	)
	tests := []struct {
		name   string
		source string
		ref    string
		want   string
	}{
		{"valid", source, testLegacyRef, ""},
		{"valid name with dashes", quarantine + "/" + testLegacyID + "-stack-feature-2", testLegacyRef, ""},
		{"valid root quarantine", "/codex-worktree-trash-20260928/" + testLegacyID + "-x", testLegacyRef, ""},
		{"ccx namespace", source, RecoveryRefFor(testID), `recovery_ref "refs/ccx/cleanup/` + testID + `"` + refShape},
		{"heads namespace", source, "refs/heads/20260928/" + testLegacyID, `recovery_ref "refs/heads/20260928/` + testLegacyID + `"` + refShape},
		{"namespace as a suffix", source, "refs/x/" + testLegacyRef, `recovery_ref "refs/x/` + testLegacyRef + `"` + refShape},
		{"empty ref", source, "", `recovery_ref ""` + refShape},
		{"date too short", source, "refs/cleanup-worktrees/2026092/" + testLegacyID, `recovery_ref "refs/cleanup-worktrees/2026092/` + testLegacyID + `"` + refShape},
		{"date too long", source, "refs/cleanup-worktrees/202609281/" + testLegacyID, `recovery_ref "refs/cleanup-worktrees/202609281/` + testLegacyID + `"` + refShape},
		{"date not digits", source, "refs/cleanup-worktrees/2026-9-28/" + testLegacyID, `recovery_ref "refs/cleanup-worktrees/2026-9-28/` + testLegacyID + `"` + refShape},
		{"id too short", source, "refs/cleanup-worktrees/20260928/" + testLegacyID[:19], `recovery_ref "refs/cleanup-worktrees/20260928/` + testLegacyID[:19] + `"` + refShape},
		{"id too long", source, testLegacyRef + "4", `recovery_ref "` + testLegacyRef + `4"` + refShape},
		{"id uppercase", source, "refs/cleanup-worktrees/20260928/" + strings.ToUpper(testLegacyID), `recovery_ref "refs/cleanup-worktrees/20260928/` + strings.ToUpper(testLegacyID) + `"` + refShape},
		{"id not hex", source, "refs/cleanup-worktrees/20260928/0123456789abcdef012g", `recovery_ref "refs/cleanup-worktrees/20260928/0123456789abcdef012g"` + refShape},
		{"ref with trailing component", source, testLegacyRef + "/x", `recovery_ref "` + testLegacyRef + `/x"` + refShape},
		{"parent not a quarantine", "/home/u/.cache/trash/" + testLegacyID + "-feature", testLegacyRef, `source "/home/u/.cache/trash/` + testLegacyID + `-feature" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"parent with a suffix", quarantine + "-old/" + testLegacyID + "-feature", testLegacyRef, `source "` + quarantine + `-old/` + testLegacyID + `-feature" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"nested below the quarantine", quarantine + "/nested/" + testLegacyID + "-feature", testLegacyRef, `source "` + quarantine + `/nested/` + testLegacyID + `-feature" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"date mismatch", "/home/u/.cache/codex-worktree-trash-20260929/" + testLegacyID + "-feature", testLegacyRef, `source "/home/u/.cache/codex-worktree-trash-20260929/` + testLegacyID + `-feature" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"quarantine itself", quarantine, testLegacyRef, `source "` + quarantine + `" is not inside a codex-worktree-trash-20260928 quarantine`},
		{"base is the bare id", quarantine + "/" + testLegacyID, testLegacyRef, `source "` + quarantine + `/` + testLegacyID + `" is not the tree ` + testLegacyRef + ` pins`},
		{"base is the id and a dash with no name", quarantine + "/" + testLegacyID + "-", testLegacyRef, `source "` + quarantine + `/` + testLegacyID + `-" is not the tree ` + testLegacyRef + ` pins`},
		{"valid one-character name", quarantine + "/" + testLegacyID + "-a", testLegacyRef, ""},
		{"valid name that is a dash", quarantine + "/" + testLegacyID + "--", testLegacyRef, ""},
		{"base of another id", quarantine + "/ffffffffffffffffffff-feature", testLegacyRef, `source "` + quarantine + `/ffffffffffffffffffff-feature" is not the tree ` + testLegacyRef + ` pins`},
		{"base extends the id", quarantine + "/" + testLegacyID + "4-feature", testLegacyRef, `source "` + quarantine + `/` + testLegacyID + `4-feature" is not the tree ` + testLegacyRef + ` pins`},
		{"base with the id inside", quarantine + "/x-" + testLegacyID + "-feature", testLegacyRef, `source "` + quarantine + `/x-` + testLegacyID + `-feature" is not the tree ` + testLegacyRef + ` pins`},
		{"base with the id uppercased", quarantine + "/" + strings.ToUpper(testLegacyID) + "-feature", testLegacyRef, `source "` + quarantine + `/` + strings.ToUpper(testLegacyID) + `-feature" is not the tree ` + testLegacyRef + ` pins`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := LegacyBinding(tt.source, tt.ref)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("LegacyBinding() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("LegacyBinding() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRequester(t *testing.T) {
	base := context.Background()
	if pid, ok := RequesterFrom(base); ok || pid != 0 {
		t.Errorf("RequesterFrom(background) = %d, %v; want 0, false", pid, ok)
	}
	tagged := WithRequester(base, 4242)
	cancelled, cancel := context.WithCancel(tagged)
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		pid  int
	}{
		{"tagged", tagged, 4242},
		{"derived", cancelled, 4242},
		{"detached", context.WithoutCancel(tagged), 4242},
		{"retagged", WithRequester(tagged, 7), 7},
		{"pid zero is still a requester", WithRequester(base, 0), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if pid, ok := RequesterFrom(tt.ctx); !ok || pid != tt.pid {
				t.Errorf("RequesterFrom() = %d, %v; want %d, true", pid, ok, tt.pid)
			}
		})
	}
	if pid, ok := RequesterFrom(base); ok || pid != 0 {
		t.Errorf("RequesterFrom(background) after tagging = %d, %v; want 0, false", pid, ok)
	}
}

func TestRefusedErrorReasons(t *testing.T) {
	reasons := []string{
		"main", "unregistered", "registered", "locked", "submodules", "nested",
		"dirty", "volume", "mismatch", "identity", "recovery", "watched",
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			refused := &RefusedError{Worktree: "/w", Reason: reason, Detail: "d"}
			if got, want := refused.Error(), "/w: d"; got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
			data, err := json.Marshal(refused)
			if want := `{"worktree":"/w","reason":"` + reason + `","detail":"d"}`; err != nil || string(data) != want {
				t.Fatalf("Marshal() = %s, %v; want %s", data, err, want)
			}
			var back RefusedError
			if err := json.Unmarshal(data, &back); err != nil || back != *refused {
				t.Errorf("Unmarshal() = %+v, %v; want %+v", back, err, *refused)
			}
		})
	}
}

func TestProcessIDJSON(t *testing.T) {
	id := ProcessID{PID: 4242, Start: 1790000000}
	data, err := json.Marshal(id)
	if want := `{"pid":4242,"start":1790000000}`; err != nil || string(data) != want {
		t.Fatalf("Marshal() = %s, %v; want %s", data, err, want)
	}
	var back ProcessID
	if err := json.Unmarshal(data, &back); err != nil || back != id {
		t.Errorf("Unmarshal() = %+v, %v; want %+v", back, err, id)
	}
	if (ProcessID{PID: 4242, Start: 1790000001}) == id {
		t.Error("a recycled pid with another start time compares equal")
	}
}

func TestRetiring(t *testing.T) {
	base := context.Background()
	if got := RetiringFrom(base); got != nil {
		t.Errorf("RetiringFrom(background) = %v, want nil", got)
	}
	watchman := ProcessID{PID: 101, Start: 1790000000}
	fsmonitor := ProcessID{PID: 202, Start: 1790000050}
	tagged := WithRetiring(base, []ProcessID{watchman, fsmonitor})
	cancelled, cancel := context.WithCancel(tagged)
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		want []ProcessID
	}{
		{"tagged", tagged, []ProcessID{watchman, fsmonitor}},
		{"derived", cancelled, []ProcessID{watchman, fsmonitor}},
		{"detached", context.WithoutCancel(tagged), []ProcessID{watchman, fsmonitor}},
		{"beside a requester", WithRequester(tagged, 7), []ProcessID{watchman, fsmonitor}},
		{"retagged", WithRetiring(tagged, []ProcessID{fsmonitor}), []ProcessID{fsmonitor}},
		{"requester alone", WithRequester(base, 7), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetiringFrom(tt.ctx); !slices.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
				t.Errorf("RetiringFrom() = %#v, want %#v", got, tt.want)
			}
		})
	}
	if got := RetiringFrom(WithRetiring(tagged, nil)); got != nil {
		t.Errorf("RetiringFrom(retagged with nil) = %#v, want nil", got)
	}
	if got := RetiringFrom(WithRetiring(base, []ProcessID{})); got == nil || len(got) != 0 {
		t.Errorf("RetiringFrom(empty) = %#v, want an empty non-nil slice", got)
	}
	if pid, ok := RequesterFrom(tagged); ok || pid != 0 {
		t.Errorf("RequesterFrom(retiring only) = %d, %v; want 0, false", pid, ok)
	}
	if got := RetiringFrom(base); got != nil {
		t.Errorf("RetiringFrom(background) after tagging = %v, want nil", got)
	}
}

func assertValidate(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		return
	}
	if err == nil || err.Error() != want {
		t.Fatalf("Validate() = %v, want %q", err, want)
	}
}
