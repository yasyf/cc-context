package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const fakePRRepo = "yasyf/cc-context"

// fakePRCreateURL is the one pull request payload still hand-modeled: gh pr
// create prints nothing but the URL of a pull request it just opened, so
// recording one opens a real pull request (`task record-gh -- --write OWNER/N`).
const fakePRCreateURL = "https://github.com/yasyf/cc-context/pull/12"

// shipPRFixture builds a real repository with an edit waiting and a bare origin
// to push it to, the shape every pull request test ships from, and puts the
// recorded gh in front of it.
func shipPRFixture(t *testing.T, opts ...vcstest.Opt) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, append([]vcstest.Opt{vcstest.Remote(), vcstest.Dirty()}, opts...)...)
	writeShipGH(t, f)
	return f
}

// prFromListGolden is the pull request a recorded gh pr list resolves to, so an
// assertion names the number and URL GitHub returned rather than ones written
// here.
func prFromListGolden(t *testing.T, scenario string) prState {
	t.Helper()
	var prs []prState
	if err := json.Unmarshal([]byte(ghStdout(t, scenario)), &prs); err != nil {
		t.Fatalf("golden %s: %v", scenario, err)
	}
	if len(prs) != 1 {
		t.Fatalf("golden %s holds %d pull requests, want 1", scenario, len(prs))
	}
	return prs[0]
}

// shipPRPushed is the git lane's plan, commit, and push, the argv every pull
// request test shares before its own gh calls. The remote-tracking ref for a
// branch never pushed does not resolve, so the ancestry check behind it never
// runs.
func shipPRPushed(branch string) [][]string {
	return [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "config", "--get", "branch." + branch + ".remote"},
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/" + branch},
		{"git", "push", "origin", branch},
	}
}

// runShipCmdStdin runs ship with an explicit input stream, which every
// --pr-body-file - test needs: cobra otherwise hands stdinPiped the test
// binary's own stdin.
func runShipCmdStdin(t *testing.T, in io.Reader, args ...string) (string, error) {
	t.Helper()
	cmd := newShipCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(in)
	cmd.SetArgs(args)
	err := cmd.Execute()
	summary := out.String()
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	return summary, err
}

// writePRBody drops a body fixture and returns its path, which is what the
// argv assertions expect to see in --body-file.
func writePRBody(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// seedPRViews points the fake gh at one canned pr view payload per branch, so a
// downstack resolves different pull requests for different branches.
func seedPRViews(t *testing.T, payloads map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for branch, payload := range payloads {
		if err := os.WriteFile(filepath.Join(dir, branch), []byte(payload), 0o600); err != nil {
			t.Fatalf("seed pr view %s: %v", branch, err)
		}
	}
	t.Setenv("GH_PR_VIEW_DIR", dir)
}

// assertNoPRStep fails on any gh verb only the pull request step issues. The
// batched lookup behind the gt lane's submit report is deliberately not one —
// it is a gh api graphql call, and every verb named here mutates or lists.
func assertNoPRStep(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if len(inv) < 3 || inv[0] != "gh" || inv[1] != "pr" {
			continue
		}
		switch inv[2] {
		case "list", "create", "edit", "ready":
			t.Errorf("pull request step ran without a pr flag: %v", inv)
		}
	}
}

func TestShipPRCreateGitLane(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-empty"))
	t.Setenv("GH_PR_CREATE_OUT", fakePRCreateURL)
	bodyDump := filepath.Join(t.TempDir(), "body")
	t.Setenv("GH_PR_BODY_DUMP", bodyDump)
	const bodyText = "why this change\n"
	body := writePRBody(t, "body.md", bodyText)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "Better title", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	assertInvocations(t, invocations, append(shipPRPushed("feature"),
		[]string{"gh", "pr", "list", "--repo", fakePRRepo, "--head", "feature", "--state", "open", "--json", "number,url,isDraft", "--limit", "1"},
		[]string{"gh", "pr", "create", "--repo", fakePRRepo, "--head", "feature", "--base", "main", "--title", "Better title", "--body-file", body},
	))
	if want := shipCommitted(t, f, vcs.Git) + " · pushed feature → origin · opened PR #12 " + fakePRCreateURL; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if got := readFileStr(t, bodyDump); got != bodyText {
		t.Errorf("gh read a body of %q, want %q", got, bodyText)
	}
	if n := remoteCount(t, f, "feature"); n != 2 {
		t.Errorf("origin feature holds %d commits, want the pushed one on top of init", n)
	}
}

func TestShipMessageFromPRFlags(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-empty"))
	t.Setenv("GH_PR_CREATE_OUT", fakePRCreateURL)
	body := writePRBody(t, "body.md", "## Context\n\nThe widget broke.\n\n<details>\n<summary>Design</summary>\n\n## Details\n\nRewrote it.\n</details>\n")

	if _, err := runShipCmd(t, "--no-watch", "--pr-title", "fix: 🐛 frobnicate the widget", "--pr-body-file", body); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "fix: 🐛 frobnicate the widget\n\nContext: The widget broke.\n\nDetails: Rewrote it."
	if got := gitAt(t, f.Dir, "log", "-1", "--format=%B"); got != want {
		t.Errorf("commit message = %q, want %q", got, want)
	}
}

func TestShipMessageFromPRTitleAlone(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-empty"))
	t.Setenv("GH_PR_CREATE_OUT", fakePRCreateURL)

	if _, err := runShipCmd(t, "--no-watch", "--pr-title", "fix: frobnicate"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if got := gitAt(t, f.Dir, "log", "-1", "--format=%B"); got != "fix: frobnicate" {
		t.Errorf("commit message = %q, want the title alone", got)
	}

	_, err := runShipCmd(t, "--no-watch", "--pr-title", "other=fix: frobnicate")
	if err == nil || err.Error() != errShipMessageRequired.Error() {
		t.Errorf("error = %v, want %v — a scoped title is not the tip's", err, errShipMessageRequired)
	}
}

func TestShipMessageFromPRBodyStdin(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-empty"))
	t.Setenv("GH_PR_CREATE_OUT", fakePRCreateURL)
	bodyDump := filepath.Join(t.TempDir(), "body")
	t.Setenv("GH_PR_BODY_DUMP", bodyDump)
	const bodyText = "## Context\n\nThe widget broke.\n"

	if _, err := runShipCmdStdin(t, strings.NewReader(bodyText), "--no-watch", "--pr-title", "fix: frobnicate", "--pr-body-file", "-"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if want := "fix: frobnicate\n\nContext: The widget broke."; gitAt(t, f.Dir, "log", "-1", "--format=%B") != want {
		t.Errorf("commit message = %q, want %q", gitAt(t, f.Dir, "log", "-1", "--format=%B"), want)
	}
	if got := readFileStr(t, bodyDump); got != bodyText {
		t.Errorf("gh read a body of %q, want the stdin body verbatim", got)
	}
}

func TestCommitBodyFromPR(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: "", want: ""},
		{name: "plain prose survives", body: "why this change\n", want: "why this change"},
		{
			name: "a heading labels the paragraph under it",
			body: "## Context\n\nThe widget broke.\nBadly.\n",
			want: "Context: The widget broke.\nBadly.",
		},
		{
			name: "the details wrapper drops away",
			body: "<details>\n<summary>Changes</summary>\n\nRewrote it.\n</details>\n",
			want: "Rewrote it.",
		},
		{
			name: "blank runs collapse",
			body: "one\n\n\n\ntwo\n\n\n",
			want: "one\n\ntwo",
		},
		{
			name: "a heading with nothing under it drops",
			body: "## Empty\n\n## Context\n\nprose\n",
			want: "Context: prose",
		},
		{
			name: "crlf leaves no carriage return on the prose",
			body: "## Context\r\n\r\nThe widget broke.\r\nBadly.\r\n",
			want: "Context: The widget broke.\nBadly.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commitBodyFromPR(tt.body); got != tt.want {
				t.Errorf("commitBodyFromPR() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestShipPRCreateDefaults pins the two defaults a create falls back on: the
// commit subject as the title, and an explicitly empty body. gh pr create
// --fill would publish the Claude-Session-Id trailer, so it is never used.
func TestShipPRCreateDefaults(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-empty"))
	t.Setenv("GH_PR_CREATE_OUT", fakePRCreateURL)
	t.Setenv(envClaudeSessionKey, "0d1e2f30-4a5b-6c7d-8e9f-a0b1c2d3e4f5")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--draft"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var create []string
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) > 2 && inv[0] == "gh" && inv[2] == "create" {
			create = inv
		}
	}
	want := []string{
		"gh", "pr", "create", "--repo", fakePRRepo, "--head", "feature", "--base", "main",
		"--title", "fix: frobnicate", "--body", "", "--draft",
	}
	if !reflect.DeepEqual(create, want) {
		t.Errorf("create argv = %v, want %v", create, want)
	}
	if slices.Contains(create, "--fill") {
		t.Error("gh pr create --fill would publish the commit's Claude-Session-Id trailer")
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
		t.Errorf("commit subject = %q, want the title the create defaulted to", subject)
	}
	if body := gitAt(t, f.Dir, "log", "-1", "--format=%b"); !strings.Contains(body, "Claude-Session-Id: 0d1e2f30-4a5b-6c7d-8e9f-a0b1c2d3e4f5") {
		t.Errorf("commit body = %q, want the session trailer --fill would have published", body)
	}
}

func TestShipPREditOnlyStatedFields(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	pr := prFromListGolden(t, "pr-list-found")
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-found"))
	body := writePRBody(t, "body.md", "regenerated\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var prCalls [][]string
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if inv[0] == "gh" && inv[1] == "pr" {
			prCalls = append(prCalls, inv)
		}
	}
	assertInvocations(t, prCalls, [][]string{
		{"gh", "pr", "list", "--repo", fakePRRepo, "--head", "feature", "--state", "open", "--json", "number,url,isDraft", "--limit", "1"},
		{"gh", "pr", "edit", strconv.Itoa(pr.Number), "--repo", fakePRRepo, "--body-file", body},
	})
	want := fmt.Sprintf("%s · pushed feature → origin · updated PR #%d %s (body)", shipCommitted(t, f, vcs.Git), pr.Number, pr.URL)
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := remoteCount(t, f, "feature"); n != 2 {
		t.Errorf("origin feature holds %d commits, want the pushed one on top of init", n)
	}
}

func TestShipPRBodyFromStdin(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	pr := prFromListGolden(t, "pr-list-found")
	t.Setenv("GH_PR_LIST_JSON", ghStdout(t, "pr-list-found"))
	bodyDump := filepath.Join(t.TempDir(), "body")
	t.Setenv("GH_PR_BODY_DUMP", bodyDump)
	const piped = "piped body\n"

	if _, err := runShipCmdStdin(t, strings.NewReader(piped), "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", "-"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var edit []string
	for _, inv := range normalizeTempPaths(vcstest.Invocations(t, f.ArgvLog)) {
		if len(inv) > 2 && inv[0] == "gh" && inv[2] == "edit" {
			edit = inv
		}
	}
	want := []string{"gh", "pr", "edit", strconv.Itoa(pr.Number), "--repo", fakePRRepo, "--body-file", "<pr-body>"}
	if !reflect.DeepEqual(edit, want) {
		t.Errorf("edit argv = %v, want %v", edit, want)
	}
	if got := readFileStr(t, bodyDump); got != piped {
		t.Errorf("gh read a body of %q, want the piped %q", got, piped)
	}
	if n := remoteCount(t, f, "feature"); n != 2 {
		t.Errorf("origin feature holds %d commits, want the pushed one on top of init", n)
	}
}

// TestShipPRGTWritesTheNewestPR proves a branch resubmitted after its first
// pull request merged has its body written onto the live one. Verified against
// github.com/yasyf/cc-context, whose yasyf/transcript-ccx-issues head carries
// PRs #1 and #2: gh pr view resolves #2, the newest, and overwriting the
// predecessor would leave the pull request under review bodyless.
func TestShipPRGTWritesTheNewestPR(t *testing.T) {
	log := setupShipGT(t, true)
	seedPRViews(t, map[string]string{"feature": `{"number":3,"url":"https://github.com/x/pull/3","body":"the merged predecessor"}` + "\n" +
		`{"number":9,"url":"https://github.com/x/pull/9","body":""}`})
	body := writePRBody(t, "body.md", "why this change\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · submitted feature → PR #9 https://github.com/x/pull/9 · set PR #9 body`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var edit []string
	for _, inv := range readInvocations(t, log) {
		if len(inv) > 2 && inv[0] == "gh" && inv[2] == "edit" {
			edit = inv
		}
	}
	wantEdit := []string{"gh", "pr", "edit", "9", "--repo", fakePRRepo, "--body-file", body}
	if !reflect.DeepEqual(edit, wantEdit) {
		t.Errorf("edit argv = %v, want %v", edit, wantEdit)
	}
}

func TestShipPRGTBothFlags(t *testing.T) {
	log := setupShipGT(t, true)
	seedPRViews(t, map[string]string{"feature": `{"number":7,"url":"https://github.com/x/pull/7","body":""}`})
	body := writePRBody(t, "body.md", "why this change\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "Better title", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · submitted feature → PR #7 https://github.com/x/pull/7 · set PR #7 title+body`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		nogtProbe,
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"gt", "add", "--no-interactive", "-A"},
		{"git", "diff", "--cached", "--quiet"},
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"gt", "state"},
		{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", "--publish"},
		ghDownstackPRArgv("feature"),
		{"gh", "pr", "edit", "7", "--repo", fakePRRepo, "--title", "Better title", "--body-file", body},
	})
}

// TestShipPRGTBackfill covers the case the branch-scoped flags exist for: one
// gt submit opens a pull request for the whole downstack with no body, and only
// the branches this invocation named get one written.
func TestShipPRGTBackfill(t *testing.T) {
	log := setupShipGT(t, true)
	t.Setenv("GIT_BRANCH", "feature2")
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},`+
		`"base":{"parents":[{"ref":"main","sha":"deadbeef"}]},`+
		`"feature":{"parents":[{"ref":"base","sha":"beadfeed"}]},`+
		`"feature2":{"parents":[{"ref":"feature","sha":"feedface"}]}}`)
	seedPRViews(t, map[string]string{
		"base":     `{"number":5,"url":"https://github.com/x/pull/5","body":"written by hand"}`,
		"feature":  `{"number":6,"url":"https://github.com/x/pull/6","body":""}`,
		"feature2": `{"number":7,"url":"https://github.com/x/pull/7","body":""}`,
	})
	tipBody := writePRBody(t, "tip.md", "tip body\n")
	midBody := writePRBody(t, "mid.md", "mid body\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch",
		"--pr-body-file", tipBody, "--pr-body-file", "feature="+midBody)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · submitted feature2 → PR #7 https://github.com/x/pull/7 ` +
		`(stack of 3: base, feature, feature2) · set PR #7 body, PR #6 body`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var gh [][]string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" {
			gh = append(gh, inv)
		}
	}
	assertInvocations(t, gh, [][]string{
		ghDownstackPRArgv("base", "feature", "feature2"),
		{"gh", "pr", "edit", "7", "--repo", fakePRRepo, "--body-file", tipBody},
		{"gh", "pr", "edit", "6", "--repo", fakePRRepo, "--body-file", midBody},
	})
}

// TestShipPRUnusedCostsNothing is the phase's price tag: a ship that names no
// pull request flag issues none of the step's gh verbs, in either lane.
func TestShipPRUnusedCostsNothing(t *testing.T) {
	t.Run("git lane", func(t *testing.T) {
		f := shipPRFixture(t, vcstest.Branch("feature"))
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertNoPRStep(t, vcstest.Invocations(t, f.ArgvLog))
		if n := remoteCount(t, f, "feature"); n != 2 {
			t.Errorf("origin feature holds %d commits, want the pushed one on top of init", n)
		}
	})
	t.Run("gt lane", func(t *testing.T) {
		log := setupShipGT(t, true)
		t.Setenv("GH_PR_VIEW_JSON", `{"number":7,"url":"https://github.com/x/pull/7"}`)
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := readInvocations(t, log)
		assertNoPRStep(t, invocations)
		var gh [][]string
		for _, inv := range invocations {
			if inv[0] == "gh" {
				gh = append(gh, inv)
			}
		}
		assertInvocations(t, gh, [][]string{ghDownstackPRArgv("feature")})
	})
	t.Run("--no-pr", func(t *testing.T) {
		f := shipPRFixture(t, vcstest.Branch("feature"))
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--no-pr", "--draft"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertNoPRStep(t, vcstest.Invocations(t, f.ArgvLog))
		if n := remoteCount(t, f, "feature"); n != 2 {
			t.Errorf("origin feature holds %d commits, want the pushed one on top of init", n)
		}
	})
}

func TestShipPROnTrunk(t *testing.T) {
	f := shipPRFixture(t)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "Better title")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertNoPRStep(t, vcstest.Invocations(t, f.ArgvLog))
	if want := shipCommitted(t, f, vcs.Git) + " · pushed main → origin · no PR (on trunk)"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the pushed one on top of init", n)
	}
}

func TestShipPRDraftTransitions(t *testing.T) {
	tests := []struct {
		name     string
		scenario string
		flag     string
		undo     bool
		wantSeg  string
	}{
		{name: "publish to draft", scenario: "pr-list-found", flag: "--draft", undo: true, wantSeg: "draft"},
		{name: "draft to ready", scenario: "pr-list-draft", flag: "--publish", wantSeg: "ready"},
		{name: "already in the stated state", scenario: "pr-list-found", flag: "--publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipPRFixture(t, vcstest.Branch("feature"))
			pr := prFromListGolden(t, tt.scenario)
			t.Setenv("GH_PR_LIST_JSON", ghStdout(t, tt.scenario))

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", tt.flag)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var ready []string
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if len(inv) > 2 && inv[0] == "gh" && inv[2] == "ready" {
					ready = inv
				}
			}
			var wantReady []string
			want := shipCommitted(t, f, vcs.Git) + " · pushed feature → origin"
			if tt.wantSeg != "" {
				wantReady = []string{"gh", "pr", "ready", strconv.Itoa(pr.Number), "--repo", fakePRRepo}
				if tt.undo {
					wantReady = append(wantReady, "--undo")
				}
				want += fmt.Sprintf(" · updated PR #%d %s (%s)", pr.Number, pr.URL, tt.wantSeg)
			}
			if !reflect.DeepEqual(ready, wantReady) {
				t.Errorf("ready argv = %v, want %v", ready, wantReady)
			}
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if n := remoteCount(t, f, "feature"); n != 2 {
				t.Errorf("origin feature holds %d commits, want the pushed one on top of init", n)
			}
		})
	}
}

func TestShipPRRefusals(t *testing.T) {
	t.Run("unreadable body file refuses before the commit", func(t *testing.T) {
		f := shipPRFixture(t, vcstest.Branch("feature"))
		head := shipHead(t, f)
		shipResetLog(t, f)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--pr-body-file", "/nonexistent/body.md")
		if err == nil || !strings.HasPrefix(err.Error(), "ship: --pr-body-file /nonexistent/body.md:") {
			t.Errorf("error = %v, want a --pr-body-file refusal", err)
		}
		assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
		assertShipRefusedClean(t, f, head)
	})

	t.Run("stdin body from a terminal", func(t *testing.T) {
		f := shipPRFixture(t, vcstest.Branch("feature"))
		head := shipHead(t, f)
		shipResetLog(t, f)
		tty, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		t.Cleanup(func() { _ = tty.Close() })
		_, err = runShipCmdStdin(t, tty, "-m", "fix: frobnicate", "--pr-body-file", "-")
		wantErr := `ship: --pr-body-file - reads the body from stdin, which is a terminal — pipe the body in or pass a path`
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
		assertShipRefusedClean(t, f, head)
	})

	t.Run("stdin claimed twice", func(t *testing.T) {
		f := shipPRFixture(t)
		head := shipHead(t, f)
		_, err := runShipCmdStdin(t, strings.NewReader("body\n"), "-m", "fix: frobnicate",
			"--pr-body-file", "-", "--pr-body-file", "feature=-")
		wantErr := `ship: only one --pr-body-file may read stdin ("-")`
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		assertShipRefusedClean(t, f, head)
	})

	t.Run("same branch named twice", func(t *testing.T) {
		f := shipPRFixture(t)
		head := shipHead(t, f)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--pr-title", "one", "--pr-title", "two")
		wantErr := "ship: --pr-title given twice for branch main"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		assertShipRefusedClean(t, f, head)
	})

	t.Run("--no-push has no pull request to touch", func(t *testing.T) {
		f := shipPRFixture(t, vcstest.Branch("feature"))
		head := shipHead(t, f)
		shipResetLog(t, f)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--pr-title", "one")
		wantErr := "ship: --pr-title/--pr-body-file require push (drop --no-push)"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := vcstest.Invocations(t, f.ArgvLog); inv != nil {
			t.Errorf("no VCS command may run before the flag check, got %v", inv)
		}
		assertShipRefusedClean(t, f, head)
	})

	t.Run("--no-pr excludes the pr flags", func(t *testing.T) {
		f := shipPRFixture(t, vcstest.Branch("feature"))
		head := shipHead(t, f)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-pr", "--pr-title", "one")
		wantErr := "if any flags in the group [no-pr pr-title] are set none of the others can be; [no-pr pr-title] were all set"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		assertShipRefusedClean(t, f, head)
	})
}

// assertShipRefusedClean proves a refusal left the repository where it stood:
// HEAD unmoved and the edit still pending, which is what a mutation the argv
// log cannot see would disturb.
func assertShipRefusedClean(t *testing.T, f *vcstest.Fixture, head string) {
	t.Helper()
	if got := shipHead(t, f); got != head {
		t.Errorf("HEAD moved to %s, want the pre-ship %s", got, head)
	}
	if status := gitAt(t, f.Dir, "status", "--porcelain"); status != "M f.txt" {
		t.Errorf("working copy = %q, want the untouched edit", status)
	}
}

func TestSplitPRValue(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		wantBranch string
		wantRest   string
	}{
		{"bare", "Better title", "tip", "Better title"},
		{"scoped", "base=Base title", "base", "Base title"},
		{"bare title carrying an equals", "set x=1 in the config", "tip", "set x=1 in the config"},
		{"illegal branch prefix stays bare", "..=weird", "tip", "..=weird"},
		{"empty prefix stays bare", "=leading", "tip", "=leading"},
		{"scoped to an empty value", "base=", "base", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, rest := splitPRValue(tt.value, "tip")
			if branch != tt.wantBranch || rest != tt.wantRest {
				t.Errorf("splitPRValue(%q) = (%q, %q), want (%q, %q)", tt.value, branch, rest, tt.wantBranch, tt.wantRest)
			}
		})
	}
}

func TestPRNumberFromURL(t *testing.T) {
	if n, err := prNumberFromURL("https://github.com/yasyf/cc-context/pull/12"); err != nil || n != 12 {
		t.Errorf("prNumberFromURL = (%d, %v), want (12, nil)", n, err)
	}
	if _, err := prNumberFromURL("https://github.com/yasyf/cc-context"); err == nil {
		t.Error("a URL with no pull request path must refuse")
	}
}
