package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const fakePRRepo = "yasyf/cc-context"

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

// assertNoPRStep fails on any gh verb only the pull request step issues. The gt
// lane's own gh pr view (the submit's report segment) is deliberately not one.
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
	log := setupShip(t, ".git", true)
	t.Setenv("GIT_BRANCH", "feature")
	t.Setenv("GIT_TRUNK", "main")
	t.Setenv("GH_PR_CREATE_OUT", "https://github.com/x/pull/12")
	body := writePRBody(t, "body.md", "why this change\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "Better title", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed feature → origin · opened PR #12 https://github.com/x/pull/12`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "config", "--get", "branch.feature.remote"},
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/feature"},
		{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/feature", "HEAD"},
		{"git", "push", "origin", "feature"},
		{"gh", "pr", "list", "--repo", fakePRRepo, "--head", "feature", "--state", "open", "--json", "number,url,isDraft", "--limit", "1"},
		{"gh", "pr", "create", "--repo", fakePRRepo, "--head", "feature", "--base", "main", "--title", "Better title", "--body-file", body},
	})
}

// TestShipPRCreateDefaults pins the two defaults a create falls back on: the
// commit subject as the title, and an explicitly empty body. gh pr create
// --fill would publish the Claude-Session-Id trailer, so it is never used.
func TestShipPRCreateDefaults(t *testing.T) {
	log := setupShip(t, ".git", true)
	t.Setenv("GIT_BRANCH", "feature")
	t.Setenv("GIT_TRUNK", "main")
	t.Setenv("GH_PR_CREATE_OUT", "https://github.com/x/pull/12")
	t.Setenv(envClaudeSessionKey, "0d1e2f30-4a5b-6c7d-8e9f-a0b1c2d3e4f5")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--draft"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	// The fakes' NUL argv log cannot carry an empty field — --body's empty value
	// reads as the record terminator — so the create record ends at --body and
	// --draft lands in the continuation.
	invocations := readInvocations(t, log)
	var create []string
	var flat []string
	for _, inv := range invocations {
		if len(inv) > 2 && inv[0] == "gh" && inv[2] == "create" {
			create = inv
		}
		flat = append(flat, inv...)
	}
	want := []string{
		"gh", "pr", "create", "--repo", fakePRRepo, "--head", "feature", "--base", "main",
		"--title", "fix: frobnicate", "--body",
	}
	if !reflect.DeepEqual(create, want) {
		t.Errorf("create argv = %v, want %v", create, want)
	}
	if !slices.Contains(flat, "--draft") {
		t.Error("--draft never reached gh pr create")
	}
	if slices.Contains(flat, "--fill") {
		t.Error("gh pr create --fill would publish the commit's Claude-Session-Id trailer")
	}
}

func TestShipPREditOnlyStatedFields(t *testing.T) {
	log := setupShip(t, ".git", true)
	t.Setenv("GIT_BRANCH", "feature")
	t.Setenv("GIT_TRUNK", "main")
	t.Setenv("GH_PR_LIST_JSON", `[{"number":12,"url":"https://github.com/x/pull/12","isDraft":false}]`)
	body := writePRBody(t, "body.md", "regenerated\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed feature → origin · updated PR #12 https://github.com/x/pull/12 (body)`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var pr [][]string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" && inv[1] == "pr" {
			pr = append(pr, inv)
		}
	}
	assertInvocations(t, pr, [][]string{
		{"gh", "pr", "list", "--repo", fakePRRepo, "--head", "feature", "--state", "open", "--json", "number,url,isDraft", "--limit", "1"},
		{"gh", "pr", "edit", "12", "--repo", fakePRRepo, "--body-file", body},
	})
}

func TestShipPRBodyFromStdin(t *testing.T) {
	log := setupShip(t, ".git", true)
	t.Setenv("GIT_BRANCH", "feature")
	t.Setenv("GIT_TRUNK", "main")
	t.Setenv("GH_PR_LIST_JSON", `[{"number":12,"url":"https://github.com/x/pull/12","isDraft":false}]`)

	if _, err := runShipCmdStdin(t, strings.NewReader("piped body\n"), "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", "-"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var edit []string
	for _, inv := range normalizeTempPaths(readInvocations(t, log)) {
		if len(inv) > 2 && inv[0] == "gh" && inv[2] == "edit" {
			edit = inv
		}
	}
	want := []string{"gh", "pr", "edit", "12", "--repo", fakePRRepo, "--body-file", "<pr-body>"}
	if !reflect.DeepEqual(edit, want) {
		t.Errorf("edit argv = %v, want %v", edit, want)
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
		{"gh", "pr", "view", "feature", "--json", "number,url"},
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"gh", "pr", "view", "feature", "--json", "number,url,body"},
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
	var pr [][]string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" && inv[1] == "pr" {
			pr = append(pr, inv)
		}
	}
	assertInvocations(t, pr, [][]string{
		{"gh", "pr", "view", "feature2", "--json", "number,url"},
		{"gh", "pr", "view", "base", "--json", "number,url,body"},
		{"gh", "pr", "view", "feature", "--json", "number,url,body"},
		{"gh", "pr", "view", "feature2", "--json", "number,url,body"},
		{"gh", "pr", "edit", "7", "--repo", fakePRRepo, "--body-file", tipBody},
		{"gh", "pr", "edit", "6", "--repo", fakePRRepo, "--body-file", midBody},
	})
}

// TestShipPRUnusedCostsNothing is the phase's price tag: a ship that names no
// pull request flag issues none of the step's gh verbs, in either lane.
func TestShipPRUnusedCostsNothing(t *testing.T) {
	t.Run("git lane", func(t *testing.T) {
		log := setupShip(t, ".git", true)
		t.Setenv("GIT_BRANCH", "feature")
		t.Setenv("GIT_TRUNK", "main")
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertNoPRStep(t, readInvocations(t, log))
	})
	t.Run("gt lane", func(t *testing.T) {
		log := setupShipGT(t, true)
		t.Setenv("GH_PR_VIEW_JSON", `{"number":7,"url":"https://github.com/x/pull/7"}`)
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertNoPRStep(t, readInvocations(t, log))
	})
	t.Run("--no-pr", func(t *testing.T) {
		log := setupShip(t, ".git", true)
		t.Setenv("GIT_BRANCH", "feature")
		t.Setenv("GIT_TRUNK", "main")
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--no-pr", "--draft"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertNoPRStep(t, readInvocations(t, log))
	})
}

func TestShipPROnTrunk(t *testing.T) {
	log := setupShip(t, ".git", true)
	t.Setenv("GIT_BRANCH", "main")
	t.Setenv("GIT_TRUNK", "main")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "Better title")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin · no PR (on trunk)`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertNoPRStep(t, readInvocations(t, log))
}

func TestShipPRDraftTransitions(t *testing.T) {
	tests := []struct {
		name    string
		listPR  string
		flag    string
		wantPR  []string
		wantSeg string
	}{
		{
			name:    "publish to draft",
			listPR:  `[{"number":12,"url":"https://github.com/x/pull/12","isDraft":false}]`,
			flag:    "--draft",
			wantPR:  []string{"gh", "pr", "ready", "12", "--repo", fakePRRepo, "--undo"},
			wantSeg: " · updated PR #12 https://github.com/x/pull/12 (draft)",
		},
		{
			name:    "draft to ready",
			listPR:  `[{"number":12,"url":"https://github.com/x/pull/12","isDraft":true}]`,
			flag:    "--publish",
			wantPR:  []string{"gh", "pr", "ready", "12", "--repo", fakePRRepo},
			wantSeg: " · updated PR #12 https://github.com/x/pull/12 (ready)",
		},
		{
			name:   "already in the stated state",
			listPR: `[{"number":12,"url":"https://github.com/x/pull/12","isDraft":false}]`,
			flag:   "--publish",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", true)
			t.Setenv("GIT_BRANCH", "feature")
			t.Setenv("GIT_TRUNK", "main")
			t.Setenv("GH_PR_LIST_JSON", tt.listPR)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", tt.flag)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `committed a1b2c3d "fix: frobnicate" · pushed feature → origin` + tt.wantSeg
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			var ready []string
			for _, inv := range readInvocations(t, log) {
				if len(inv) > 2 && inv[0] == "gh" && inv[2] == "ready" {
					ready = inv
				}
			}
			if !reflect.DeepEqual(ready, tt.wantPR) {
				t.Errorf("ready argv = %v, want %v", ready, tt.wantPR)
			}
		})
	}
}

func TestShipPRRefusals(t *testing.T) {
	t.Run("unreadable body file refuses before the commit", func(t *testing.T) {
		log := setupShip(t, ".git", true)
		t.Setenv("GIT_BRANCH", "feature")
		t.Setenv("GIT_TRUNK", "main")
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--pr-body-file", "/nonexistent/body.md")
		if err == nil || !strings.HasPrefix(err.Error(), "ship: --pr-body-file /nonexistent/body.md:") {
			t.Errorf("error = %v, want a --pr-body-file refusal", err)
		}
		assertNoShipMutation(t, readInvocations(t, log))
	})

	t.Run("stdin body from a terminal", func(t *testing.T) {
		log := setupShip(t, ".git", true)
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
		assertNoShipMutation(t, readInvocations(t, log))
	})

	t.Run("stdin claimed twice", func(t *testing.T) {
		setupShip(t, ".git", true)
		_, err := runShipCmdStdin(t, strings.NewReader("body\n"), "-m", "fix: frobnicate",
			"--pr-body-file", "-", "--pr-body-file", "feature=-")
		wantErr := `ship: only one --pr-body-file may read stdin ("-")`
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
	})

	t.Run("same branch named twice", func(t *testing.T) {
		setupShip(t, ".git", true)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--pr-title", "one", "--pr-title", "two")
		wantErr := "ship: --pr-title given twice for branch main"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
	})

	t.Run("--no-push has no pull request to touch", func(t *testing.T) {
		log := setupShip(t, ".git", true)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--pr-title", "one")
		wantErr := "ship: --pr-title/--pr-body-file require push (drop --no-push)"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := readInvocations(t, log); inv != nil {
			t.Errorf("no VCS command may run before the flag check, got %v", inv)
		}
	})

	t.Run("--no-pr excludes the pr flags", func(t *testing.T) {
		setupShip(t, ".git", true)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-pr", "--pr-title", "one")
		wantErr := "if any flags in the group [no-pr pr-title] are set none of the others can be; [no-pr pr-title] were all set"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
	})
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
