package cli

import (
	"context"
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
	write(filepath.Join(filepath.Dir(repoPath), "gt.json"), fmt.Sprintf(
		`{"schema":1,"reachable":%t,"note":%q,"fetched_at":%q}`, !seed.unreachable, seed.note, now))
}

// clearLaneRecords empties dir's lane cache, so the gate has to look the repo
// up rather than read a seeded verdict.
func clearLaneRecords(t *testing.T, dir string) {
	t.Helper()
	repoPath, err := vcs.RepoCachePath(dir)
	if err != nil {
		t.Fatalf("resolve repo cache path: %v", err)
	}
	for _, path := range []string{repoPath, filepath.Join(filepath.Dir(repoPath), "gt.json")} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear %s: %v", path, err)
		}
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
	tests := []struct {
		name      string
		output    string
		code      int
		reachable bool
		note      string
		known     bool
	}{
		{
			name:   "synced repo",
			output: "Authenticated as: yasyf\n✅ Ready to submit PRs to github.com/yasyf/cc-context\n",
			code:   0, reachable: true, known: true,
		},
		{
			name:   "authenticated but outside a submittable repo",
			output: "Authenticated as: yasyf\n",
			code:   0,
		},
		{
			name:   "not synced",
			output: "Error: yasyf does not have the necessary permissions to submit PRs to cli/cli\n",
			code:   1,
			note:   "Error: yasyf does not have the necessary permissions to submit PRs to cli/cli",
			known:  true,
		},
		{
			name:   "no token",
			output: "No auth token set\n",
			code:   1,
			note:   "graphite has no auth token — run gt auth --token <token>",
			known:  true,
		},
		{
			name:   "server unreachable",
			output: "Could not connect to the Graphite server\n",
			code:   1,
		},
		{
			name:   "unrecognized failure demotes on gt's own words",
			output: "gt: something new went wrong\n",
			code:   1,
			note:   "gt: something new went wrong",
			known:  true,
		},
		{
			name:  "silent nonzero exit still demotes",
			code:  1,
			note:  "gt auth failed without output",
			known: true,
		},
		{
			name:   "a reworded ready line is not a success",
			output: "✅ All set to open PRs against github.com/yasyf/cc-context\n",
			code:   0,
		},
		{
			name:   "an overlong failure is capped",
			output: strings.Repeat("z", gtProbeNoteBudget+50),
			code:   1,
			note:   strings.Repeat("z", gtProbeNoteBudget) + "…",
			known:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reachable, note, known := classifyGTProbe(tt.output, tt.code)
			if reachable != tt.reachable || note != tt.note || known != tt.known {
				t.Errorf("classifyGTProbe() = (%v, %q, %v), want (%v, %q, %v)",
					reachable, note, known, tt.reachable, tt.note, tt.known)
			}
		})
	}
}

// TestShipGateProbe drives the probe end to end through ship, with the cached
// verdict cleared so gt auth actually runs.
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
		{name: "server unreachable", stdout: "", exit: "1", stderr: gtProbeUnreadable},
		{name: "authenticated outside a repo", stdout: "Authenticated as: yasyf"},
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

// TestShipGateProbeTimeoutKeepsGT proves a hung probe never costs the gt lane:
// demoting on a network stall would push a branch outside a live stack.
func TestShipGateProbeTimeoutKeepsGT(t *testing.T) {
	log := setupShipGT(t, false)
	clearLaneRecords(t, ".")
	t.Setenv("GT_AUTH_HANG", "1")

	start := time.Now()
	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if elapsed < gtProbeTimeout {
		t.Errorf("ship took %s, want at least the %s probe timeout — the probe did not hang", elapsed, gtProbeTimeout)
	}
	if elapsed > 4*gtProbeTimeout {
		t.Errorf("ship took %s, want the probe bounded near %s", elapsed, gtProbeTimeout)
	}
	invocations := readInvocations(t, log)
	if len(invocations) < 2 || invocations[1][0] != "gt" || invocations[1][1] != "auth" {
		t.Fatalf("argv after the lane gate = %v, want gt auth", invocations)
	}
	if strings.HasPrefix(out, "lane ") {
		t.Errorf("report = %q, want no lane segment on an unknown verdict", out)
	}
	assertGTCommit(t, invocations)
}

// TestGTReachabilityCaches proves the probe runs once per repo, that an unknown
// verdict is never cached, and that a negative expires on the shorter TTL.
func TestGTReachabilityCaches(t *testing.T) {
	tests := []struct {
		name      string
		reachable bool
		age       time.Duration
		wantFresh bool
	}{
		{"fresh positive", true, time.Hour, true},
		{"positive within 24h", true, 23 * time.Hour, true},
		{"positive past 24h", true, 25 * time.Hour, false},
		{"negative within 1h", false, 30 * time.Minute, true},
		{"negative past 1h", false, 2 * time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
			dir := t.TempDir()
			path, err := gtCachePath(dir)
			if err != nil {
				t.Fatalf("gt cache path: %v", err)
			}
			body := fmt.Sprintf(`{"schema":1,"reachable":%t,"note":"n","fetched_at":%q}`,
				tt.reachable, time.Now().Add(-tt.age).Format(time.RFC3339Nano))
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("seed gt record: %v", err)
			}
			if _, ok := readGTRecord(path); ok != tt.wantFresh {
				t.Errorf("readGTRecord fresh = %v, want %v", ok, tt.wantFresh)
			}
		})
	}
}

func TestGTReachabilityDoesNotCacheUnknown(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()

	reachable, note, err := gtReachability(context.Background(), dir, false)
	if err != nil || !reachable || note != "" {
		t.Fatalf("gtReachability() = (%v, %q, %v), want (true, \"\", nil)", reachable, note, err)
	}
	path, err := gtCachePath(dir)
	if err != nil {
		t.Fatalf("gt cache path: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("gt.json exists after an unknown verdict (stat err = %v), want no record", statErr)
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
			reachable, note, err := gtReachability(context.Background(), dir, false)
			if err != nil || !reachable || note != "" {
				t.Errorf("gtReachability() = (%v, %q, %v), want (true, \"\", nil)", reachable, note, err)
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
