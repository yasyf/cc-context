package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcs"
)

// laneSeed overrides the fields seedLaneRecords writes; the zero value seeds a
// private repo the viewer administers, with gt reachable.
type laneSeed struct {
	nameWithOwner string
	owner         string
	public        bool
	permission    string
	unaffiliated  bool
	unreachable   bool
	note          string
}

// seedLaneRecords writes dir's cached GitHub metadata and gt-reachability
// verdict, so the lane gate resolves from cache without a gh subprocess. The
// JSON mirrors the envelopes internal/vcs and lane.go persist.
func seedLaneRecords(t *testing.T, dir string, seed laneSeed) {
	t.Helper()
	repoPath, err := vcs.RepoCachePath(dir)
	if err != nil {
		t.Fatalf("resolve repo cache path: %v", err)
	}
	if seed.nameWithOwner == "" {
		seed.nameWithOwner = "yasyf/cc-context"
	}
	if seed.owner == "" {
		seed.owner = "yasyf"
	}
	if seed.permission == "" {
		seed.permission = "ADMIN"
	}
	now := time.Now().Format(time.RFC3339Nano)
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	write(repoPath, fmt.Sprintf(
		`{"schema":1,"repo":{"name_with_owner":%q,"owner":%q,"is_private":%t,"viewer_login":"yasyf","viewer_permission":%q,"affiliated":%t,"fetched_at":%q}}`,
		seed.nameWithOwner, seed.owner, !seed.public, seed.permission, !seed.unaffiliated, now))
	verdict := gtVerdictOK
	if seed.unreachable {
		verdict = gtVerdictDenied
	}
	write(filepath.Join(filepath.Dir(repoPath), "gt.json"), fmt.Sprintf(
		`{"schema":%d,"verdict":%q,"note":%q,"fetched_at":%q}`, gtSchema, verdict, seed.note, now))
}

// clearLaneRecords empties dir's lane cache, so the gate has to look the repo
// up rather than read a seeded verdict.
func clearLaneRecords(t *testing.T, dir string) {
	t.Helper()
	repoPath, err := vcs.RepoCachePath(dir)
	if err != nil {
		t.Fatalf("resolve repo cache path: %v", err)
	}
	if err := os.Remove(repoPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear %s: %v", repoPath, err)
	}
	clearGTRecord(t, dir)
}

// clearGTRecord drops just the cached gt verdict, leaving the seeded GitHub
// record in place. A test that wants a live probe still wants repo metadata
// served from cache: clearing it too sends the report back to the fake gh,
// failing the test on an unrelated lookup.
func clearGTRecord(t *testing.T, dir string) {
	t.Helper()
	repoPath, err := vcs.RepoCachePath(dir)
	if err != nil {
		t.Fatalf("resolve repo cache path: %v", err)
	}
	path := filepath.Join(filepath.Dir(repoPath), "gt.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear %s: %v", path, err)
	}
}

// nogtProbe is the ccx.nogt read the lane gate makes before committing to the
// graphite lane, so it leads every gt-lane argv sequence.
var nogtProbe = []string{"git", "config", "--get", nogtKey}

// foreignRepo seeds a public repository the viewer neither owns, administers,
// nor shares an organization with.
var foreignRepo = laneSeed{nameWithOwner: "cli/cli", owner: "cli", public: true, permission: "READ", unaffiliated: true}

const foreignNote = "cli/cli is public and you have read access"

func assertNoGT(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if inv[0] == "gt" {
			t.Errorf("gt ran on a demoted lane: %v", inv)
		}
	}
}

func assertGTCommit(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if len(inv) > 1 && inv[0] == "gt" && (inv[1] == "create" || inv[1] == "modify") {
			return
		}
	}
	t.Errorf("no gt create/modify in %v, want the graphite lane", invocations)
}

func TestShipGateDemotesForeignRepo(t *testing.T) {
	log := setupShipGT(t, false)
	seedLaneRecords(t, ".", foreignRepo)

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "lane git (" + foreignNote + ")"
	if !strings.HasPrefix(out, want) {
		t.Errorf("report = %q, want it to lead with %q", out, want)
	}
	invocations := readInvocations(t, log)
	assertNoGTCommit(t, invocations)
	assertNoGT(t, invocations)
}

func TestShipGateKeepsOwnRepo(t *testing.T) {
	tests := []struct {
		name string
		seed laneSeed
	}{
		{"private repo read-only", laneSeed{permission: "READ", unaffiliated: true}},
		{"admin on a public repo", laneSeed{public: true, permission: "ADMIN", unaffiliated: true}},
		{"maintainer on a public repo", laneSeed{public: true, permission: "MAINTAIN", unaffiliated: true}},
		{"affiliated public repo", laneSeed{public: true, permission: "READ"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			seedLaneRecords(t, ".", tt.seed)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if strings.HasPrefix(got, "lane ") {
				t.Errorf("summary = %q, want no lane segment on an undemoted ship", got)
			}
			assertGTCommit(t, readInvocations(t, log))
		})
	}
}

// TestShipGateUnknownKeepsGT proves an unanswerable lookup — no gh at all —
// leaves the graphite lane alone: unknown is never read as "not yours".
func TestShipGateUnknownKeepsGT(t *testing.T) {
	log := setupShipGT(t, false)
	clearLaneRecords(t, ".")
	if path, err := exec.LookPath("gh"); err == nil {
		t.Fatalf("gh resolved to %s; this test must run with none on PATH", path)
	}

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := readInvocations(t, log)
	assertGTCommit(t, invocations)
	for _, inv := range invocations {
		if inv[0] == "gh" {
			t.Errorf("gh ran with none on PATH: %v", inv)
		}
	}
}

func TestClassifyGTProbe(t *testing.T) {
	const unconfirmed = "gt auth exited 0 without confirming this repo is submittable"
	tests := []struct {
		name    string
		output  string
		code    int
		verdict gtVerdict
		note    string
	}{
		{
			name:    "synced repo",
			output:  "Authenticated as: yasyf\n✅ Ready to submit PRs to github.com/yasyf/cc-context\n",
			code:    0,
			verdict: gtVerdictOK,
		},
		{
			name:    "authenticated but outside a submittable repo",
			output:  "Authenticated as: yasyf\n",
			code:    0,
			verdict: gtVerdictUnknown,
			note:    unconfirmed,
		},
		{
			name:    "not synced",
			output:  "Error: yasyf does not have the necessary permissions to submit PRs to cli/cli\n",
			code:    1,
			verdict: gtVerdictDenied,
			note:    "Error: yasyf does not have the necessary permissions to submit PRs to cli/cli",
		},
		{
			name:    "no token",
			output:  "No auth token set\n",
			code:    1,
			verdict: gtVerdictDenied,
			note:    "graphite has no auth token — run gt auth --token <token>",
		},
		{
			name:    "server unreachable",
			output:  "Could not connect to the Graphite server\n",
			code:    1,
			verdict: gtVerdictUnknown,
			note:    "graphite server unreachable",
		},
		{
			name:    "unrecognized failure demotes on gt's own words",
			output:  "gt: something new went wrong\n",
			code:    1,
			verdict: gtVerdictDenied,
			note:    "gt: something new went wrong",
		},
		{
			name:    "silent nonzero exit still demotes",
			code:    1,
			verdict: gtVerdictDenied,
			note:    "gt auth failed without output",
		},
		{
			name:    "a reworded ready line is not a success",
			output:  "✅ All set to open PRs against github.com/yasyf/cc-context\n",
			code:    0,
			verdict: gtVerdictUnknown,
			note:    unconfirmed,
		},
		{
			name:    "an overlong failure is capped",
			output:  strings.Repeat("z", gtProbeNoteBudget+50),
			code:    1,
			verdict: gtVerdictDenied,
			note:    strings.Repeat("z", gtProbeNoteBudget) + "…",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict, note := classifyGTProbe(tt.output, tt.code)
			if verdict != tt.verdict || note != tt.note {
				t.Errorf("classifyGTProbe() = (%q, %q), want (%q, %q)", verdict, note, tt.verdict, tt.note)
			}
		})
	}
}

// TestShipGateProbe drives the probe end to end through ship, with the cached
// verdict cleared so gt auth actually runs. Only gt's own ready line keeps the
// lane; every other answer, decline or non-answer alike, demotes with a note.
func TestShipGateProbe(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		stderr   string
		exit     string
		wantNote string
	}{
		{name: "reachable", stdout: "✅ Ready to submit PRs to github.com/yasyf/cc-context"},
		{
			name: "not synced", stdout: "", exit: "1",
			stderr:   "Error: yasyf does not have the necessary permissions to submit PRs to cli/cli",
			wantNote: "Error: yasyf does not have the necessary permissions to submit PRs to cli/cli",
		},
		{
			name: "server unreachable", stdout: "", exit: "1", stderr: gtProbeUnreadable,
			wantNote: "graphite server unreachable",
		},
		{
			name: "authenticated outside a repo", stdout: "Authenticated as: yasyf",
			wantNote: "gt auth exited 0 without confirming this repo is submittable",
		},
		{
			name: "unrecognized failure", stdout: "", exit: "1",
			stderr:   "Error: gt rewrote this message in 2.0",
			wantNote: "Error: gt rewrote this message in 2.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			clearLaneRecords(t, ".")
			t.Setenv("GT_AUTH_STDOUT", tt.stdout)
			t.Setenv("GT_AUTH_STDERR", tt.stderr)
			t.Setenv("GT_AUTH_EXIT", tt.exit)

			out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			invocations := readInvocations(t, log)
			if len(invocations) < 2 || invocations[1][0] != "gt" || invocations[1][1] != "auth" {
				t.Fatalf("argv after the lane gate = %v, want gt auth", invocations)
			}
			if tt.wantNote == "" {
				assertGTCommit(t, invocations)
				if strings.HasPrefix(out, "lane ") {
					t.Errorf("report = %q, want no lane segment", out)
				}
				return
			}
			want := "lane git (" + tt.wantNote + ")"
			if !strings.HasPrefix(out, want) {
				t.Errorf("report = %q, want it to lead with %q", out, want)
			}
			assertNoGTCommit(t, invocations)
		})
	}
}

// TestShipGateProbeTimeoutDemotes proves a hung probe costs the gt lane. Riding
// the lane on an answer nobody got is the worse trade: it submits into a stack
// Graphite may not accept, where demoting only lands a plain branch, and the
// report names the reason so the demotion is never silent.
func TestShipGateProbeTimeoutDemotes(t *testing.T) {
	log := setupShipGT(t, false)
	clearLaneRecords(t, ".")
	t.Setenv("GT_AUTH_HANG", "1")

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "lane git (gt auth did not answer within " + gtProbeTimeout.String() + ")"
	if !strings.HasPrefix(out, want) {
		t.Errorf("report = %q, want it to lead with %q", out, want)
	}
	assertNoGTCommit(t, readInvocations(t, log))
}

// TestGTReachabilityCaches proves each verdict is served for its own TTL: a
// positive for a day, a negative for an hour, and an unknown for a minute.
func TestGTReachabilityCaches(t *testing.T) {
	tests := []struct {
		name      string
		schema    int
		verdict   gtVerdict
		age       time.Duration
		wantFresh bool
	}{
		{"fresh positive", gtSchema, gtVerdictOK, time.Hour, true},
		{"positive within 24h", gtSchema, gtVerdictOK, 23 * time.Hour, true},
		{"positive past 24h", gtSchema, gtVerdictOK, 25 * time.Hour, false},
		{"negative within 1h", gtSchema, gtVerdictDenied, 30 * time.Minute, true},
		{"negative past 1h", gtSchema, gtVerdictDenied, 2 * time.Hour, false},
		{"unknown within 60s", gtSchema, gtVerdictUnknown, 30 * time.Second, true},
		{"unknown past 60s", gtSchema, gtVerdictUnknown, 2 * time.Minute, false},
		{"a boolean schema-1 record reads as a miss", 1, gtVerdictOK, time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
			dir := t.TempDir()
			path, err := gtCachePath(dir)
			if err != nil {
				t.Fatalf("gt cache path: %v", err)
			}
			body := fmt.Sprintf(`{"schema":%d,"verdict":%q,"note":"n","fetched_at":%q}`,
				tt.schema, tt.verdict, time.Now().Add(-tt.age).Format(time.RFC3339Nano))
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("seed gt record: %v", err)
			}
			if _, ok := readGTRecord(path); ok != tt.wantFresh {
				t.Errorf("readGTRecord fresh = %v, want %v", ok, tt.wantFresh)
			}
		})
	}
}

// TestGTReachabilityCachesUnknownBriefly proves an unanswerable probe demotes and
// is remembered, but only for gtUnknownTTL. The two halves are one decision:
// unknown demotes, so a cached unknown is a cached demotion — it cannot resurrect
// the lane it just declined, which is what makes storing it safe at all, and
// storing it stops every command during an outage paying a fresh probe to
// re-derive the same answer. The short TTL is the other half: the one verdict
// nobody could confirm is re-asked as soon as the answer might have changed, so a
// repo that recovers is not held demoted.
func TestGTReachabilityCachesUnknownBriefly(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()

	verdict, note, err := gtReachability(context.Background(), dir, false)
	if err != nil || verdict != gtVerdictUnknown || note == "" {
		t.Fatalf("gtReachability() = (%q, %q, %v), want (%q, a reason, nil)", verdict, note, err, gtVerdictUnknown)
	}
	path, err := gtCachePath(dir)
	if err != nil {
		t.Fatalf("gt cache path: %v", err)
	}
	rec, ok := readGTRecord(path)
	if !ok || rec.Verdict != gtVerdictUnknown {
		t.Fatalf("readGTRecord() = (%+v, %v), want a stored %q verdict", rec, ok, gtVerdictUnknown)
	}

	rec.FetchedAt = time.Now().Add(-gtUnknownTTL - time.Second)
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal backdated record: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write backdated record: %v", err)
	}
	if _, ok := readGTRecord(path); ok {
		t.Errorf("readGTRecord() served a record older than gtUnknownTTL (%s), want a miss", gtUnknownTTL)
	}
}

// TestGTReachabilityLocksProbe proves two concurrent probes of one repo cost a
// single gt auth: whoever loses the lock serves the winner's stored verdict
// instead of paying for the round trip again.
func TestGTReachabilityLocksProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	bin := t.TempDir()
	// Wide enough that the second caller is inside the lock while the first
	// probes; PATH is the fake bin dir alone, so sleep needs its absolute path.
	fake := "#!/bin/sh\nprintf 'probe\\n' >> \"$GT_PROBE_LOG\"\n/bin/sleep 0.3\n" +
		"printf '%s\\n' '" + gtProbeReady + " github.com/yasyf/cc-context'\n"
	if err := os.WriteFile(filepath.Join(bin, "gt"), []byte(fake), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake gt: %v", err)
	}
	probes := filepath.Join(t.TempDir(), "probes")
	t.Setenv("PATH", bin)
	t.Setenv("GT_PROBE_LOG", probes)

	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdict, note, err := gtReachability(context.Background(), dir, false)
			if err != nil || verdict != gtVerdictOK || note != "" {
				t.Errorf("gtReachability() = (%q, %q, %v), want (%q, \"\", nil)", verdict, note, err, gtVerdictOK)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(probes)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	if n := strings.Count(string(data), "probe\n"); n != 1 {
		t.Errorf("gt auth probed %d times, want 1 — the verdict is stored outside the lock", n)
	}
}

func TestShipGateRespectsNoGTConfig(t *testing.T) {
	log := setupShipGT(t, false)
	t.Setenv("GIT_CONFIG_CCX_NOGT", "true")

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "lane git (gt disabled for this repo (ccx.nogt))"
	if !strings.HasPrefix(out, want) {
		t.Errorf("report = %q, want it to lead with %q", out, want)
	}
	assertNoGT(t, readInvocations(t, log))
}

// brokenCheckoutDir builds a working copy whose .git file points at an admin dir
// that is not there — the shape left behind when a linked worktree's repository
// is deleted out from under it — and returns the symlinked path a caller would
// spell alongside the canonical root every report must name. The two differ by
// construction: a fixture already canonical could not catch a root that skipped
// canonicalization.
func brokenCheckoutDir(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "wt")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatalf("create checkout: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink checkout: %v", err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %q: %v", dir, err)
	}
	if link == root {
		t.Fatalf("fixture %q is already canonical — the test could not fail", link)
	}
	target := filepath.Join(root, "gone", "worktrees", "wt")
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+target+"\n"), 0o600); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}
	return link, root
}

// TestResolveLaneBrokenCheckout proves the split the two resolvers exist for. A
// checkout whose pointer into its repository resolves to nothing errors, which
// is all a mutating command reads; the lane comes back carrying the backend and
// working copy that did resolve, so a reporting command has something to
// diagnose. The zero lane would not do — vcs.Git is 0, so it reports a git
// checkout nobody resolved. Both roots come off the partial checkout, so the
// report's root line and its checkout line name one path in one spelling.
func TestResolveLaneBrokenCheckout(t *testing.T) {
	dir, root := brokenCheckoutDir(t)
	target := filepath.Join(root, "gone", "worktrees", "wt")

	refused, err := resolveLane(context.Background(), "ship", dir, false)
	var broken *vcs.BrokenCheckout
	if !errors.As(err, &broken) {
		t.Fatalf("resolveLane() error = %v, want a *vcs.BrokenCheckout", err)
	}
	if refused.broken == nil || refused.kind != vcs.Git || refused.root != root {
		t.Errorf("resolveLane() lane = %+v, want the diagnosis over (%v, %q)", refused, vcs.Git, root)
	}

	l, err := resolveLaneReport(context.Background(), "info", dir, false, false)
	if err != nil {
		t.Fatalf("resolveLaneReport() error = %v, want nil", err)
	}
	if l.broken == nil {
		t.Fatalf("resolveLaneReport() lane = %+v, want it to carry the broken checkout", l)
	}
	if l.kind != vcs.Git || l.root != root {
		t.Errorf("resolveLaneReport() lane = (%v, %q), want (%v, %q)", l.kind, l.root, vcs.Git, root)
	}
	if l.root != l.broken.Root {
		t.Errorf("lane root %q and diagnosis root %q are one path in two spellings", l.root, l.broken.Root)
	}
	if l.broken.Target != target {
		t.Errorf("broken.Target = %q, want %q", l.broken.Target, target)
	}
	if l.gt {
		t.Error("resolveLaneReport() kept the gt lane over a broken checkout")
	}
}

// TestResolveLaneNoRepository proves the Kind None rejection stays here:
// ResolveCheckout answers "no repository" without failing, and a directory
// outside one is refused by every caller, the reporting ones included.
func TestResolveLaneNoRepository(t *testing.T) {
	const want = "info: no git or jj repository in the working directory"
	l, err := resolveLaneReport(context.Background(), "info", t.TempDir(), false, false)
	if err == nil || err.Error() != want {
		t.Fatalf("resolveLaneReport() error = %v, want %q", err, want)
	}
	if l != (lane{}) {
		t.Errorf("resolveLaneReport() lane = %+v, want the zero lane", l)
	}
}

// TestKindLabel pins a label for every kind, None included: a reported broken
// checkout can carry one now, and a panic is not a diagnosis.
func TestKindLabel(t *testing.T) {
	tests := []struct {
		kind vcs.Kind
		want string
	}{
		{vcs.Git, "git"},
		{vcs.JJ, "jj"},
		{vcs.None, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := kindLabel(tt.kind); got != tt.want {
				t.Errorf("kindLabel(%d) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

// TestReviewsStackDeclinesForeignRepo proves the gate is shared: a demoted ship
// never builds a stack, so reviews --stack must not go looking for one.
func TestReviewsStackDeclinesForeignRepo(t *testing.T) {
	setupShipGT(t, false)
	seedLaneRecords(t, ".", foreignRepo)

	_, err := runReviewsCmd(t, "--stack")
	wantErr := "reviews: --stack declined the graphite lane: " + foreignNote
	if err == nil || err.Error() != wantErr {
		t.Fatalf("reviews --stack error = %v, want %q", err, wantErr)
	}
}
