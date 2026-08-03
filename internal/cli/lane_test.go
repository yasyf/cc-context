package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
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

// clearRepoRecord drops just the cached GitHub metadata, leaving the seeded gt
// verdict in place, so the gate has to look the repository up while the probe
// still answers from cache.
func clearRepoRecord(t *testing.T, dir string) {
	t.Helper()
	repoPath, err := vcs.RepoCachePath(dir)
	if err != nil {
		t.Fatalf("resolve repo cache path: %v", err)
	}
	if err := os.Remove(repoPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear %s: %v", repoPath, err)
	}
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

// shortenGTProbe cuts the probe deadline for a test that waits it out, so a
// hung gt auth costs a second rather than the field-derived cap. Still far
// longer than spawning gt, so the probe is aborted mid-run — the path under
// test — never before it starts.
func shortenGTProbe(t *testing.T) {
	t.Helper()
	prior := gtProbeTimeout
	gtProbeTimeout = time.Second
	t.Cleanup(func() { gtProbeTimeout = prior })
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
	f := shipGTFeature(t)
	seedLaneRecords(t, ".", foreignRepo)

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "lane git (" + foreignNote + ")"
	if !strings.HasPrefix(out, want) {
		t.Errorf("report = %q, want it to lead with %q", out, want)
	}
	invocations := shipGTInvocations(t, f)
	assertNoGTCommit(t, invocations)
	assertNoGT(t, invocations)
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
		t.Errorf("HEAD subject = %q, want the commit the demoted lane cut itself", subject)
	}
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
			f := shipGTFeature(t)
			seedLaneRecords(t, ".", tt.seed)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if strings.HasPrefix(got, "lane ") {
				t.Errorf("summary = %q, want no lane segment on an undemoted ship", got)
			}
			assertGTCommit(t, shipGTInvocations(t, f))
			if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
				t.Errorf("HEAD subject = %q, want the commit gt cut on feature", subject)
			}
		})
	}
}

// TestShipGateUnknownKeepsGT proves an unanswerable lookup — no gh at all —
// leaves the graphite lane alone: unknown is never read as "not yours". Only
// the repository record is cleared; the shim directory alone is PATH, since a
// system one carries a gh the lookup would then answer from.
func TestShipGateUnknownKeepsGT(t *testing.T) {
	f := shipGTFeature(t)
	clearRepoRecord(t, ".")
	f.OnlyShimPATH(t)
	if path, err := exec.LookPath("gh"); err == nil {
		t.Fatalf("gh resolved to %s; this test must run with none on PATH", path)
	}

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := shipGTInvocations(t, f)
	assertGTCommit(t, invocations)
	for _, inv := range invocations {
		if inv[0] == "gh" {
			t.Errorf("gh ran with none on PATH: %v", inv)
		}
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
		t.Errorf("HEAD subject = %q, want the commit gt cut on feature", subject)
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
// verdict cleared so gt auth actually runs and answers out of the recorded
// corpus. Every recorded answer demotes — a decline, an unreachable server, and
// an exit 0 that never confirms this repository alike — each carrying its own
// note into the report, and the ship lands on the git lane regardless.
//
// The ready line that would keep the lane has no recording: gt prints it only
// for a repository Graphite is permitted to submit to, so testdata/gt names it
// NOT RECORDED and the positive arm lives in TestShipGateKeepsOwnRepo, off a
// seeded verdict.
func TestShipGateProbe(t *testing.T) {
	tests := []struct {
		golden   string
		wantNote string
	}{
		{golden: "auth-no-token", wantNote: "graphite has no auth token — run gt auth --token <token>"},
		{
			golden:   "auth-no-perms",
			wantNote: "ERROR: Graphite does not have the necessary permissions to submit PRs to yasyf/cc-context.",
		},
		{golden: "auth-unreachable", wantNote: "graphite server unreachable"},
		{golden: "auth-authenticated-elsewhere", wantNote: "gt auth exited 0 without confirming this repo is submittable"},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			g := loadGTGolden(t, tt.golden)
			f := shipGTFeature(t)
			clearGTRecord(t, ".")
			shipGTAuth(t, f, g)

			out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			invocations := shipGTInvocations(t, f)
			if len(invocations) < 2 || invocations[1][0] != "gt" || invocations[1][1] != "auth" {
				t.Fatalf("argv after the lane gate = %v, want gt auth", invocations)
			}
			want := "lane git (" + tt.wantNote + ")"
			if !strings.HasPrefix(out, want) {
				t.Errorf("report = %q, want it to lead with %q", out, want)
			}
			assertNoGTCommit(t, invocations)
			if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
				t.Errorf("HEAD subject = %q, want the commit the demoted lane cut itself", subject)
			}
		})
	}
}

// TestShipGateProbeTimeoutDemotes proves a hung probe costs the gt lane. Riding
// the lane on an answer nobody got is the worse trade: it submits into a stack
// Graphite may not accept, where demoting only lands a plain branch, and the
// report names the reason so the demotion is never silent.
func TestShipGateProbeTimeoutDemotes(t *testing.T) {
	f := shipGTFeature(t)
	clearGTRecord(t, ".")
	shipGTAuthHang(t, f)
	shortenGTProbe(t)

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "lane git (gt auth did not answer within " + gtProbeTimeout.String() + ")"
	if !strings.HasPrefix(out, want) {
		t.Errorf("report = %q, want it to lead with %q", out, want)
	}
	assertNoGTCommit(t, shipGTInvocations(t, f))
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
		t.Errorf("HEAD subject = %q, want the commit the demoted lane cut itself", subject)
	}
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

// TestShipGateRespectsNoGTConfig drives the gate over a real graphite repository
// whose ccx.nogt is set with git itself: the opt-out is a git-config read, so a
// knob standing in for one proves nothing about the key git actually answers
// for. The demotion costs no gt at all — the gate reads the config before it
// would ask Graphite anything.
func TestShipGateRespectsNoGTConfig(t *testing.T) {
	f := vcstest.Repo(t, vcstest.GT(), vcstest.Remote())
	seedLaneRecords(t, ".", laneSeed{})
	runTool(t, f.Dir, "git", "config", nogtKey, "true")
	resetArgvLog(t, f)

	l, err := resolveLane(context.Background(), "ship", f.Dir, false)
	if err != nil {
		t.Fatalf("resolveLane() error = %v", err)
	}
	if l.gt {
		t.Error("resolveLane() kept the gt lane over a set ccx.nogt")
	}
	if want := "gt disabled for this repo (" + nogtKey + ")"; l.note != want {
		t.Errorf("lane note = %q, want %q", l.note, want)
	}
	vcstest.Quiesce(t, f.ArgvLog)
	invocations := vcstest.Invocations(t, f.ArgvLog)
	assertNoGT(t, invocations)
	if want := [][]string{nogtProbe}; !reflect.DeepEqual(invocations, want) {
		t.Errorf("invocations = %v, want %v — the gate stops at the config read", invocations, want)
	}
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
	f := shipGTFeature(t)
	seedLaneRecords(t, ".", foreignRepo)
	head := shipHead(t, f)

	_, err := runReviewsCmd(t, "--stack")
	wantErr := "reviews: --stack declined the graphite lane: " + foreignNote
	if err == nil || err.Error() != wantErr {
		t.Fatalf("reviews --stack error = %v, want %q", err, wantErr)
	}
	assertShipRefusedClean(t, f, head)
}
