package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	fakeGuidelinesRepoView = `{"nameWithOwner":"acme/widget",` +
		`"pullRequestTemplates":[{"filename":"PULL_REQUEST_TEMPLATE.md","body":"## Summary\n\n## Test plan\n"}],` +
		`"codeOfConduct":{"key":"other","name":"Other","url":"https://github.com/acme/widget/blob/main/CODE-OF-CONDUCT.md"},` +
		`"contactLinks":[{"about":"Ask in discussions","name":"Question","url":"https://example.test/discuss"}],` +
		`"issueTemplates":[{"name":"Bug report","title":"","about":"Report a bug","body":"steps\nexpected\n"}]}`

	fakeGuidelinesMultiTemplate = `{"nameWithOwner":"acme/widget",` +
		`"pullRequestTemplates":[{"filename":"beta.md","body":"beta body\n"},{"filename":"alpha.md","body":"alpha body\n"}],` +
		`"codeOfConduct":null,"contactLinks":[],"issueTemplates":[]}`

	fakeGuidelinesProfile = `{"files":{"contributing":` +
		`{"url":"https://api.github.com/repos/acme/widget/contents/.github/CONTRIBUTING.md"}}}`
)

// writeGuidelinesFakes installs a fake gh into dir. It records its argv into
// $GUIDELINES_LOG as a NUL-delimited record (every field terminated by \0, the
// record by one extra \0) and answers off env vars, so the guidelines command's
// parsing paths run without the network.
func writeGuidelinesFakes(t *testing.T, dir string) {
	t.Helper()
	gh := `#!/bin/sh
{ printf 'gh\0'; for a in "$@"; do printf '%s\0' "$a"; done; printf '\0'; } >> "$GUIDELINES_LOG"
case "$1" in
  repo)
    if [ -n "$GH_REPO_VIEW_FAIL" ]; then printf 'gh: could not resolve a github repository\n' >&2; exit 1; fi
    printf '%s' "$GH_GUIDELINES_VIEW_JSON" ;;
  api)
    case "$2" in
      *community/profile) printf '%s' "$GH_COMMUNITY_PROFILE_JSON" ;;
      *) printf '%s' "$GH_CONTENTS_BODY" ;;
    esac ;;
  *) printf 'fake gh: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake gh: %v", err)
	}
}

// setupGuidelines stands up a repo root with a fake gh on PATH and an isolated
// cache, and returns the root and the argv log path.
func setupGuidelines(t *testing.T, withGh bool) (root, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}

	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	binDir := t.TempDir()
	if withGh {
		writeGuidelinesFakes(t, binDir)
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	logPath = filepath.Join(binDir, "gh.log")
	t.Setenv("PATH", binDir)
	t.Setenv("GUIDELINES_LOG", logPath)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("GH_GUIDELINES_VIEW_JSON", fakeGuidelinesRepoView)
	return root, logPath
}

func writeGuidelinesFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func runGuidelinesCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newGuidelinesCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func runGuidelinesJSON(t *testing.T, args ...string) guidelines {
	t.Helper()
	out, errOut, err := runGuidelinesCmd(t, append([]string{"--json"}, args...)...)
	if err != nil {
		t.Fatalf("guidelines --json: %v (stderr %q)", err, errOut)
	}
	var g guidelines
	if err := json.Unmarshal([]byte(out), &g); err != nil {
		t.Fatalf("unmarshal guidelines: %v\n%s", err, out)
	}
	return g
}

func readGuidelinesLog(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read guidelines log: %v", err)
	}
	var records [][]string
	for _, record := range bytes.Split(data, []byte{0, 0}) {
		if len(record) == 0 {
			continue
		}
		fields := bytes.Split(record, []byte{0})
		row := make([]string, len(fields))
		for i, field := range fields {
			row[i] = string(field)
		}
		records = append(records, row)
	}
	return records
}

func countGuidelinesCalls(t *testing.T, path, subcommand string) int {
	t.Helper()
	n := 0
	for _, record := range readGuidelinesLog(t, path) {
		if len(record) > 1 && record[1] == subcommand {
			n++
		}
	}
	return n
}

func guidelinesDocOf(t *testing.T, g guidelines, kind string) guidelinesDoc {
	t.Helper()
	for _, doc := range g.Documents {
		if doc.Kind == kind {
			return doc
		}
	}
	t.Fatalf("no %s document in %+v", kind, g.Documents)
	return guidelinesDoc{}
}

func TestGuidelinesMultiTemplateDirectory(t *testing.T) {
	root, _ := setupGuidelines(t, true)
	t.Setenv("GH_GUIDELINES_VIEW_JSON", fakeGuidelinesMultiTemplate)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "be nice\n")
	writeGuidelinesFile(t, root, ".github/PULL_REQUEST_TEMPLATE/alpha.md", "alpha body\n")
	writeGuidelinesFile(t, root, ".github/PULL_REQUEST_TEMPLATE/beta.md", "beta body\n")

	g := runGuidelinesJSON(t)

	var templates []guidelinesDoc
	for _, doc := range g.Documents {
		if doc.Kind == guidelinesKindPRTemplate {
			templates = append(templates, doc)
		}
	}
	if len(templates) != 2 {
		t.Fatalf("pr-template documents = %d, want 2: %+v", len(templates), g.Documents)
	}
	want := []struct{ path, body string }{
		{".github/PULL_REQUEST_TEMPLATE/alpha.md", "alpha body\n"},
		{".github/PULL_REQUEST_TEMPLATE/beta.md", "beta body\n"},
	}
	for i, tt := range want {
		if templates[i].Path != tt.path {
			t.Errorf("template %d path = %q, want %q", i, templates[i].Path, tt.path)
		}
		if templates[i].Body != tt.body {
			t.Errorf("template %d body = %q, want %q", i, templates[i].Body, tt.body)
		}
		if templates[i].Source != guidelinesSourceGitHub {
			t.Errorf("template %d source = %q, want %q", i, templates[i].Source, guidelinesSourceGitHub)
		}
	}
}

func TestGuidelinesLocalContributingWins(t *testing.T) {
	root, logPath := setupGuidelines(t, true)
	t.Setenv("GH_COMMUNITY_PROFILE_JSON", fakeGuidelinesProfile)
	t.Setenv("GH_CONTENTS_BODY", "remote contributing\n")
	writeGuidelinesFile(t, root, ".github/CONTRIBUTING.md", "local contributing\n")

	g := runGuidelinesJSON(t)

	doc := guidelinesDocOf(t, g, guidelinesKindContributing)
	if doc.Source != guidelinesSourceLocal {
		t.Errorf("source = %q, want %q", doc.Source, guidelinesSourceLocal)
	}
	if doc.Path != ".github/CONTRIBUTING.md" {
		t.Errorf("path = %q, want %q", doc.Path, ".github/CONTRIBUTING.md")
	}
	if doc.Body != "local contributing\n" {
		t.Errorf("body = %q, want the local file", doc.Body)
	}
	if n := countGuidelinesCalls(t, logPath, "api"); n != 0 {
		t.Errorf("gh api calls = %d, want 0 — the fallback fires only on a local miss", n)
	}
}

func TestGuidelinesContributingFallback(t *testing.T) {
	_, logPath := setupGuidelines(t, true)
	t.Setenv("GH_COMMUNITY_PROFILE_JSON", fakeGuidelinesProfile)
	t.Setenv("GH_CONTENTS_BODY", "remote contributing\n")

	g := runGuidelinesJSON(t)

	doc := guidelinesDocOf(t, g, guidelinesKindContributing)
	if doc.Source != guidelinesSourceGitHub {
		t.Errorf("source = %q, want %q", doc.Source, guidelinesSourceGitHub)
	}
	if doc.Path != ".github/CONTRIBUTING.md" {
		t.Errorf("path = %q, want %q", doc.Path, ".github/CONTRIBUTING.md")
	}
	if doc.Body != "remote contributing\n" {
		t.Errorf("body = %q, want the fetched file", doc.Body)
	}
	want := [][]string{
		{"gh", "repo", "view", "--json", guidelinesRepoFields},
		{"gh", "api", "repos/acme/widget/community/profile"},
		{"gh", "api", "https://api.github.com/repos/acme/widget/contents/.github/CONTRIBUTING.md", "-H", guidelinesRawAccept},
	}
	if got := readGuidelinesLog(t, logPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv records:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestGuidelinesBudgetTruncatesPerDocument(t *testing.T) {
	root, _ := setupGuidelines(t, true)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", strings.Repeat("a line of contributing guidance\n", 40))

	g := runGuidelinesJSON(t, "--budget", "10")

	doc := guidelinesDocOf(t, g, guidelinesKindContributing)
	if !doc.Truncated {
		t.Fatalf("contributing truncated = false, want true (tokens %d)", doc.Tokens)
	}
	if doc.OmittedTokens <= 0 {
		t.Errorf("omitted tokens = %d, want > 0", doc.OmittedTokens)
	}
	if doc.Tokens > 10 {
		t.Errorf("tokens = %d, want <= the 10-token budget", doc.Tokens)
	}
	if strings.Contains(doc.Body, "omitted") {
		t.Errorf("json body carries the human footer: %q", doc.Body)
	}

	template := guidelinesDocOf(t, g, guidelinesKindPRTemplate)
	if template.Truncated {
		t.Error("pr-template truncated: the budget is per document, so a long CONTRIBUTING must not starve it")
	}

	out, _, err := runGuidelinesCmd(t, "--budget", "10")
	if err != nil {
		t.Fatalf("guidelines: %v", err)
	}
	if !strings.Contains(out, "tokens omitted — re-run with a larger --budget") {
		t.Errorf("human output missing the render.Cap footer:\n%s", out)
	}
}

func TestGuidelinesCacheWarm(t *testing.T) {
	root, logPath := setupGuidelines(t, true)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "first\n")

	first := runGuidelinesJSON(t)
	if first.Cached {
		t.Error("first run reported a cache hit")
	}

	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "second\n")
	second := runGuidelinesJSON(t)
	if !second.Cached {
		t.Error("second run reported a cache miss")
	}
	if n := countGuidelinesCalls(t, logPath, "repo"); n != 1 {
		t.Errorf("gh repo view calls = %d, want 1", n)
	}
	if doc := guidelinesDocOf(t, second, guidelinesKindContributing); doc.Body != "second\n" {
		t.Errorf("contributing body = %q, want the re-read local file", doc.Body)
	}
	if second.CachePath == "" {
		t.Error("cache_path is empty")
	}
	if _, err := os.Stat(second.CachePath); err != nil {
		t.Errorf("stat cache_path: %v", err)
	}
	if base := filepath.Base(second.CachePath); base != guidelinesFile {
		t.Errorf("cache file = %q, want %q", base, guidelinesFile)
	}
}

func TestGuidelinesRefreshRefetches(t *testing.T) {
	root, logPath := setupGuidelines(t, true)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "be nice\n")

	runGuidelinesJSON(t)
	second := runGuidelinesJSON(t, "--refresh")

	if second.Cached {
		t.Error("--refresh reported a cache hit")
	}
	if n := countGuidelinesCalls(t, logPath, "repo"); n != 2 {
		t.Errorf("gh repo view calls = %d, want 2", n)
	}
}

func TestGuidelinesWithoutGh(t *testing.T) {
	root, _ := setupGuidelines(t, false)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "be nice\n")
	writeGuidelinesFile(t, root, ".github/ISSUE_TEMPLATE/config.yml", "blank_issues_enabled: false\n")

	out, errOut, err := runGuidelinesCmd(t, "--json")
	if err != nil {
		t.Fatalf("guidelines: %v", err)
	}
	if !strings.Contains(errOut, "serving local documents only") {
		t.Errorf("stderr = %q, want the degradation note", errOut)
	}
	var g guidelines
	if err := json.Unmarshal([]byte(out), &g); err != nil {
		t.Fatalf("unmarshal guidelines: %v\n%s", err, out)
	}
	if got := len(g.Documents); got != 2 {
		t.Fatalf("documents = %d, want the 2 local ones: %+v", got, g.Documents)
	}
	want := []string{
		guidelinesKindPRTemplate,
		guidelinesKindCodeOfConduct,
		guidelinesKindContactLinks,
		guidelinesKindIssueTemplate,
	}
	if !reflect.DeepEqual(g.Missing, want) {
		t.Errorf("missing = %v, want %v", g.Missing, want)
	}
	if g.Repo != filepath.Base(root) {
		t.Errorf("repo = %q, want the directory name %q", g.Repo, filepath.Base(root))
	}
	if !g.FetchedAt.IsZero() {
		t.Errorf("fetched_at = %v, want the zero time — nothing was fetched", g.FetchedAt)
	}
}

func TestGuidelinesJSONFields(t *testing.T) {
	root, _ := setupGuidelines(t, true)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "Sign your commits with a Signed-off-by trailer.\n")

	g := runGuidelinesJSON(t)

	if g.Repo != "acme/widget" {
		t.Errorf("repo = %q, want acme/widget", g.Repo)
	}
	if g.FetchedAt.IsZero() {
		t.Error("fetched_at is the zero time")
	}
	if !reflect.DeepEqual(g.Missing, []string{guidelinesKindIssueConfig}) {
		t.Errorf("missing = %v, want [%s]", g.Missing, guidelinesKindIssueConfig)
	}
	wantSignals := map[string]bool{"signoff_required": true, "cla": false, "conventional_commits": false}
	if !reflect.DeepEqual(g.Signals, wantSignals) {
		t.Errorf("signals = %v, want %v", g.Signals, wantSignals)
	}

	coc := guidelinesDocOf(t, g, guidelinesKindCodeOfConduct)
	if coc.Path != "https://github.com/acme/widget/blob/main/CODE-OF-CONDUCT.md" {
		t.Errorf("code-of-conduct path = %q, want the reported url", coc.Path)
	}
	contact := guidelinesDocOf(t, g, guidelinesKindContactLinks)
	if contact.Body != "Question — Ask in discussions (https://example.test/discuss)\n" {
		t.Errorf("contact-links body = %q", contact.Body)
	}
}

func TestGuidelinesIssueTemplateBodies(t *testing.T) {
	root, _ := setupGuidelines(t, true)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "be nice\n")

	names := guidelinesDocOf(t, runGuidelinesJSON(t), guidelinesKindIssueTemplate)
	if names.Body != "Bug report — Report a bug\n" {
		t.Errorf("issue-template body = %q, want the names only", names.Body)
	}
	if names.Path != guidelinesIssueTemplateDir {
		t.Errorf("issue-template path = %q, want %q", names.Path, guidelinesIssueTemplateDir)
	}

	full := guidelinesDocOf(t, runGuidelinesJSON(t, "--full"), guidelinesKindIssueTemplate)
	if !strings.Contains(full.Body, "steps\nexpected") {
		t.Errorf("--full body = %q, want the template body", full.Body)
	}
}

func TestGuidelinesHumanOutput(t *testing.T) {
	root, _ := setupGuidelines(t, true)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "We follow Conventional Commits.\n")

	out, _, err := runGuidelinesCmd(t)
	if err != nil {
		t.Fatalf("guidelines: %v", err)
	}
	for _, want := range []string{
		"# guidelines acme/widget · 5 documents · fetched now\n",
		"## pr-template PULL_REQUEST_TEMPLATE.md (github · 6 tokens)\n",
		"## contributing CONTRIBUTING.md (local · 8 tokens)\n",
		"# missing: issue-config\n",
		"# signals: signoff=no cla=no conventional-commits=yes\n",
		"# cache: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	cached, _, err := runGuidelinesCmd(t)
	if err != nil {
		t.Fatalf("guidelines (warm): %v", err)
	}
	if !strings.Contains(cached, "· cached 0s ago (--refresh to re-fetch)\n") {
		t.Errorf("warm output missing the cache age:\n%s", cached)
	}
}

func newGuidelinesRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return root
}

// writeGuidelinesWorktree hand-builds the pointer files `git worktree add`
// writes — a .git file naming an admin dir under mainRoot, and that dir's
// commondir — so the layout classifies without a git binary. The pointer holds
// mainRoot resolved, because that is the form git writes.
func writeGuidelinesWorktree(t *testing.T, mainRoot string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(mainRoot)
	if err != nil {
		t.Fatalf("resolve %q: %v", mainRoot, err)
	}
	admin := filepath.Join(resolved, ".git", "worktrees", "wt")
	if err := os.MkdirAll(admin, 0o750); err != nil {
		t.Fatalf("mkdir admin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	linked := t.TempDir()
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatalf("write gitdir pointer: %v", err)
	}
	return linked
}

func mustGuidelinesCacheDir(t *testing.T, root string) string {
	t.Helper()
	dir, err := guidelinesCacheDir(root)
	if err != nil {
		t.Fatalf("guidelinesCacheDir(%q): %v", root, err)
	}
	return dir
}

func TestGuidelinesCacheDirKeysTheRepository(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	mainRoot := newGuidelinesRepo(t)
	mainDir := mustGuidelinesCacheDir(t, mainRoot)

	tests := []struct {
		name     string
		root     func(t *testing.T) string
		wantSame bool
	}{
		{"the main checkout", func(*testing.T) string { return mainRoot }, true},
		{"a linked worktree", func(t *testing.T) string { return writeGuidelinesWorktree(t, mainRoot) }, true},
		{"an unrelated repository", func(t *testing.T) string { return newGuidelinesRepo(t) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.root(t)
			dir := mustGuidelinesCacheDir(t, root)
			repoPath, err := vcs.RepoCachePath(root)
			if err != nil {
				t.Fatalf("RepoCachePath(%q): %v", root, err)
			}
			if want := filepath.Dir(repoPath); dir != want {
				t.Errorf("guidelinesCacheDir = %q, want the GitHub record's directory %q", dir, want)
			}
			if same := dir == mainDir; same != tt.wantSame {
				t.Errorf("shares the main checkout's directory = %v, want %v (%q vs %q)", same, tt.wantSame, dir, mainDir)
			}
		})
	}
}

func TestGuidelinesSignals(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]bool
	}{
		{"none", "Open a pull request.\n", map[string]bool{"signoff_required": false, "cla": false, "conventional_commits": false}},
		{"signoff", "Every commit needs a Signed-off-by trailer.\n", map[string]bool{"signoff_required": true, "cla": false, "conventional_commits": false}},
		{"cla acronym", "Sign the CLA before we merge.\n", map[string]bool{"signoff_required": false, "cla": true, "conventional_commits": false}},
		{"cla spelled out", "Sign our contributor license agreement.\n", map[string]bool{"signoff_required": false, "cla": true, "conventional_commits": false}},
		{"cla inside a word", "Please clarify the CLASSIFICATION.\n", map[string]bool{"signoff_required": false, "cla": false, "conventional_commits": false}},
		{"conventional commits", "Commit messages follow Conventional Commits.\n", map[string]bool{"signoff_required": false, "cla": false, "conventional_commits": true}},
		{"all three", "Use Conventional Commits, add Signed-off-by, and sign the CLA.\n", map[string]bool{"signoff_required": true, "cla": true, "conventional_commits": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guidelinesSignals([]guidelinesDoc{{Kind: guidelinesKindContributing, raw: tt.body}})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("guidelinesSignals(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
