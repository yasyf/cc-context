package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// guidelinesGH names the recorded gh runs one fixture's fake gh replays. A zero
// value leaves gh off PATH entirely, which is the degradation path.
type guidelinesGH struct {
	repoView     string
	profile      string
	contributing string
}

// guidelinesPopulated is cli/cli: a pull request template, a code of conduct,
// two contact links, and three issue templates.
var guidelinesPopulated = guidelinesGH{repoView: "guidelines-repo-view-populated"}

// guidelinesRecordedRepo names every guidelines golden whose recorded argv
// carries an argument production does not send, and the one to drop. Only the
// populated repo view does: `gh repo view` with no argument reads the working
// directory's repository, and there is only one of those to record from, so
// cli/cli had to be named positionally (testdata/gh/README.md § Where a
// recording deviates from production argv). Every other guidelines golden was
// captured with production's exact argv.
var guidelinesRecordedRepo = map[string]string{"guidelines-repo-view-populated": "cli/cli"}

const (
	// guidelinesArgvSep joins an invocation into the one string a shell can
	// compare against; a space could not, since one recorded argument holds one.
	guidelinesArgvSep    = "\x1f"
	guidelinesArgvSepEnv = "CCX_GH_ARGV_SEP"
)

// guidelinesProductionArgv is the argv production must send to earn g's bytes.
func guidelinesProductionArgv(t *testing.T, g ghGolden) []string {
	t.Helper()
	extra, ok := guidelinesRecordedRepo[g.name]
	if !ok {
		return g.argv
	}
	i := slices.Index(g.argv, extra)
	if i < 0 {
		t.Fatalf("golden %s argv %q carries no %q — a re-recording made its guidelinesRecordedRepo entry stale", g.name, g.argv, extra)
	}
	return slices.Delete(slices.Clone(g.argv), i, i+1)
}

// guidelinesGHArgv frames one invocation the way the shim logs it — the tool
// name, then its arguments — as the key the fake matches on.
func guidelinesGHArgv(record []string) string {
	return strings.Join(record, guidelinesArgvSep) + guidelinesArgvSep
}

// writeGuidelinesGH installs a fake gh into the fixture's shim directory. gh is
// a network boundary so the process is faked, but every byte it prints came out
// of a real gh run, and it prints them only for the argv that run was recorded
// with: an invocation matching no golden is refused rather than answered with
// bytes GitHub produced for a different request. It frames its argv the way the
// shim does, so vcstest.Invocations counts its calls beside git's.
func writeGuidelinesGH(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	script := "#!/bin/sh\n" + vcstest.RecordArgv("gh", f.ArgvLog) +
		`key="gh$` + guidelinesArgvSepEnv + `"` + "\n" +
		`for a in "$@"; do key="$key$a$` + guidelinesArgvSepEnv + `"; done` + "\n" +
		`if [ "$key" = "$CCX_GH_ARGV_REPO_VIEW" ]; then printf '%s' "$GH_GUIDELINES_VIEW_JSON"
elif [ "$key" = "$CCX_GH_ARGV_PROFILE" ]; then printf '%s' "$GH_COMMUNITY_PROFILE_JSON"
elif [ "$key" = "$CCX_GH_ARGV_CONTENTS" ]; then printf '%s' "$GH_CONTENTS_BODY"
else printf 'fake gh: no golden was recorded for argv: %s\n' "$*" >&2; exit 2
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(f.ShimBin, "gh"), []byte(script), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake gh: %v", err)
	}
}

// setupGuidelines stands a real repository up with a fake gh replaying gh's own
// recorded payloads, and returns the repository root and the shim's argv log.
// The goldens load before the fixture chdirs, since testdata is package-relative.
// A fixture that registers no repo view narrows PATH to the shim directory:
// systemPATH holds a real gh on CI, so dropping the fake is not enough to make
// gh absent.
func setupGuidelines(t *testing.T, gh guidelinesGH) (root, logPath string) {
	t.Helper()
	env := map[string]string{guidelinesArgvSepEnv: guidelinesArgvSep}
	answers := map[string]string{}
	for _, replay := range []struct{ scenario, payloadEnv, argvEnv string }{
		{gh.repoView, "GH_GUIDELINES_VIEW_JSON", "CCX_GH_ARGV_REPO_VIEW"},
		{gh.profile, "GH_COMMUNITY_PROFILE_JSON", "CCX_GH_ARGV_PROFILE"},
		{gh.contributing, "GH_CONTENTS_BODY", "CCX_GH_ARGV_CONTENTS"},
	} {
		if replay.scenario == "" {
			continue
		}
		golden := loadGHGolden(t, replay.scenario)
		key := guidelinesGHArgv(append([]string{"gh"}, guidelinesProductionArgv(t, golden)...))
		env[replay.payloadEnv], env[replay.argvEnv] = golden.stdout, key
		answers[key] = replay.scenario
	}

	f := vcstest.Repo(t)
	if gh.repoView != "" {
		writeGuidelinesGH(t, f)
		t.Cleanup(func() { assertGuidelinesGHArgv(t, f.ArgvLog, answers) })
	} else {
		f.OnlyShimPATH(t)
	}
	for name, payload := range env {
		t.Setenv(name, payload)
	}
	return f.Dir, f.ArgvLog
}

// assertGuidelinesGHArgv fails for every gh call no golden answers. The fake
// already refuses to serve one; this names the argv, which the refusal itself
// does not — the command degrades past a failed fetch.
func assertGuidelinesGHArgv(t *testing.T, log string, answers map[string]string) {
	t.Helper()
	for _, record := range guidelinesGHCalls(t, log) {
		if _, ok := answers[guidelinesGHArgv(record)]; !ok {
			t.Errorf("gh %q matches no recorded golden; this fixture replays %v", record[1:], slices.Sorted(maps.Values(answers)))
		}
	}
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

func guidelinesGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // git resolves through the fixture's shim; args are test-authored
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// guidelinesCommitContributing writes CONTRIBUTING.md and commits it, so a later
// branch switch really rewrites the file on disk.
func guidelinesCommitContributing(t *testing.T, root, body string) {
	t.Helper()
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", body)
	guidelinesGit(t, root, "add", "CONTRIBUTING.md")
	guidelinesGit(t, root, "commit", "-qm", "contributing")
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

// guidelinesGHCalls returns the fake gh's argv records, dropping the git calls
// ccx made through the same shim log.
func guidelinesGHCalls(t *testing.T, log string) [][]string {
	t.Helper()
	var records [][]string
	for _, record := range vcstest.Invocations(t, log) {
		if record[0] == "gh" {
			records = append(records, record)
		}
	}
	return records
}

func countGuidelinesCalls(t *testing.T, log, subcommand string) int {
	t.Helper()
	n := 0
	for _, record := range guidelinesGHCalls(t, log) {
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

func decodeGuidelinesRepoView(t *testing.T, scenario string) guidelinesRepoView {
	t.Helper()
	var view guidelinesRepoView
	if err := json.Unmarshal([]byte(loadGHGolden(t, scenario).stdout), &view); err != nil {
		t.Fatalf("decode %s: %v", scenario, err)
	}
	return view
}

// TestGuidelinesMultiTemplateDirectory pins the multi-template sort. No recorded
// repo view carries two templates — cli/cli has one and cc-context none — so the
// view is the recorded one with its own template body repeated under two
// filenames, built as a Go value rather than as bytes a fake gh claims gh
// printed. Recording a real one needs a repository with a
// .github/PULL_REQUEST_TEMPLATE/ directory:
// `gh repo view <repo> --json nameWithOwner,pullRequestTemplates,codeOfConduct,contactLinks,issueTemplates`.
func TestGuidelinesMultiTemplateDirectory(t *testing.T) {
	t.Parallel()
	view := decodeGuidelinesRepoView(t, "guidelines-repo-view-populated")
	body := view.PullRequestTemplates[0].Body
	view.PullRequestTemplates = []guidelinesPRTemplate{{Filename: "beta.md", Body: body}, {Filename: "alpha.md", Body: body}}

	root := t.TempDir()
	writeGuidelinesFile(t, root, ".github/PULL_REQUEST_TEMPLATE/alpha.md", body)
	writeGuidelinesFile(t, root, ".github/PULL_REQUEST_TEMPLATE/beta.md", body)

	var templates []guidelinesDoc
	docs := guidelinesDocuments(root, guidelinesEnvelope{Repo: view}, nil, false)
	for _, doc := range docs {
		if doc.Kind == guidelinesKindPRTemplate {
			templates = append(templates, doc)
		}
	}
	if len(templates) != 2 {
		t.Fatalf("pr-template documents = %d, want 2: %+v", len(templates), docs)
	}
	wantPaths := []string{".github/PULL_REQUEST_TEMPLATE/alpha.md", ".github/PULL_REQUEST_TEMPLATE/beta.md"}
	for i, want := range wantPaths {
		if templates[i].Path != want {
			t.Errorf("template %d path = %q, want %q", i, templates[i].Path, want)
		}
		if templates[i].raw != body {
			t.Errorf("template %d body = %q, want the recorded template", i, templates[i].raw)
		}
		if templates[i].Source != guidelinesSourceGitHub {
			t.Errorf("template %d source = %q, want %q", i, templates[i].Source, guidelinesSourceGitHub)
		}
	}
}

func TestGuidelinesLocalContributingWins(t *testing.T) {
	gh := guidelinesPopulated
	gh.profile, gh.contributing = "guidelines-profile-found", "guidelines-contributing-raw"
	root, logPath := setupGuidelines(t, gh)
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
	gh := guidelinesPopulated
	gh.profile, gh.contributing = "guidelines-profile-found", "guidelines-contributing-raw"
	wantBody := loadGHGolden(t, "guidelines-contributing-raw").stdout
	scenarios := []string{gh.repoView, gh.profile, gh.contributing}
	want := make([][]string, 0, len(scenarios))
	for _, scenario := range scenarios {
		want = append(want, append([]string{"gh"}, guidelinesProductionArgv(t, loadGHGolden(t, scenario))...))
	}
	_, logPath := setupGuidelines(t, gh)

	g := runGuidelinesJSON(t)

	doc := guidelinesDocOf(t, g, guidelinesKindContributing)
	if doc.Source != guidelinesSourceGitHub {
		t.Errorf("source = %q, want %q", doc.Source, guidelinesSourceGitHub)
	}
	if doc.Path != ".github/CONTRIBUTING.md" {
		t.Errorf("path = %q, want %q", doc.Path, ".github/CONTRIBUTING.md")
	}
	if doc.Body != wantBody {
		t.Errorf("body = %q, want the fetched file %q", doc.Body, wantBody)
	}
	if got := guidelinesGHCalls(t, logPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv records:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestGuidelinesBareRepoReportsEverythingMissing drives the pair of recorded
// runs a repository with no contribution documents produces: an empty repo view
// and a community profile naming no CONTRIBUTING, so the raw-contents fetch
// never happens.
func TestGuidelinesBareRepoReportsEverythingMissing(t *testing.T) {
	_, logPath := setupGuidelines(t, guidelinesGH{repoView: "guidelines-repo-view-bare", profile: "guidelines-profile-none"})

	g := runGuidelinesJSON(t)

	if g.Repo != "yasyf/cc-context" {
		t.Errorf("repo = %q, want yasyf/cc-context", g.Repo)
	}
	if len(g.Documents) != 0 {
		t.Errorf("documents = %+v, want none", g.Documents)
	}
	if !reflect.DeepEqual(g.Missing, guidelinesKinds) {
		t.Errorf("missing = %v, want every kind %v", g.Missing, guidelinesKinds)
	}
	if n := countGuidelinesCalls(t, logPath, "api"); n != 1 {
		t.Errorf("gh api calls = %d, want 1 — the profile names no contributing, so nothing is fetched", n)
	}
}

func TestGuidelinesBudgetTruncatesPerDocument(t *testing.T) {
	view := decodeGuidelinesRepoView(t, "guidelines-repo-view-populated")
	// One token over the recorded template, so it survives whole while a
	// CONTRIBUTING twice its size does not.
	budget := len(view.PullRequestTemplates[0].Body)/guidelinesCharsPerToken + 1
	root, _ := setupGuidelines(t, guidelinesPopulated)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", strings.Repeat("a line of contributing guidance\n", 2*budget*guidelinesCharsPerToken/32))

	g := runGuidelinesJSON(t, "--budget", fmt.Sprint(budget))

	doc := guidelinesDocOf(t, g, guidelinesKindContributing)
	if !doc.Truncated {
		t.Fatalf("contributing truncated = false, want true (tokens %d)", doc.Tokens)
	}
	if doc.OmittedTokens <= 0 {
		t.Errorf("omitted tokens = %d, want > 0", doc.OmittedTokens)
	}
	if doc.Tokens > budget {
		t.Errorf("tokens = %d, want <= the %d-token budget", doc.Tokens, budget)
	}
	if strings.Contains(doc.Body, "omitted") {
		t.Errorf("json body carries the human footer: %q", doc.Body)
	}

	template := guidelinesDocOf(t, g, guidelinesKindPRTemplate)
	if template.Truncated {
		t.Error("pr-template truncated: the budget is per document, so a long CONTRIBUTING must not starve it")
	}

	out, _, err := runGuidelinesCmd(t, "--budget", fmt.Sprint(budget))
	if err != nil {
		t.Fatalf("guidelines: %v", err)
	}
	if !strings.Contains(out, "tokens omitted — re-run with a larger --budget") {
		t.Errorf("human output missing the render.Cap footer:\n%s", out)
	}
}

// TestGuidelinesCacheWarm proves the contract the envelope is shaped around: the
// GitHub payload caches for a day while the local documents re-read every run.
// The second read happens on another branch, so git — not the test — is what
// rewrote CONTRIBUTING.md on disk between them.
func TestGuidelinesCacheWarm(t *testing.T) {
	root, logPath := setupGuidelines(t, guidelinesPopulated)
	guidelinesCommitContributing(t, root, "first\n")
	guidelinesGit(t, root, "switch", "-qc", "other")
	guidelinesCommitContributing(t, root, "second\n")
	guidelinesGit(t, root, "switch", "-q", "main")

	first := runGuidelinesJSON(t)
	if first.Cached {
		t.Error("first run reported a cache hit")
	}
	if doc := guidelinesDocOf(t, first, guidelinesKindContributing); doc.Body != "first\n" {
		t.Errorf("contributing body on main = %q, want %q", doc.Body, "first\n")
	}

	guidelinesGit(t, root, "switch", "-q", "other")
	second := runGuidelinesJSON(t)
	if !second.Cached {
		t.Error("second run reported a cache miss")
	}
	if n := countGuidelinesCalls(t, logPath, "repo"); n != 1 {
		t.Errorf("gh repo view calls = %d, want 1", n)
	}
	if doc := guidelinesDocOf(t, second, guidelinesKindContributing); doc.Body != "second\n" {
		t.Errorf("contributing body on other = %q, want the branch's own file", doc.Body)
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
	root, logPath := setupGuidelines(t, guidelinesPopulated)
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
	root, _ := setupGuidelines(t, guidelinesGH{})
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
	root, _ := setupGuidelines(t, guidelinesPopulated)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "Sign your commits with a Signed-off-by trailer.\n")

	g := runGuidelinesJSON(t)

	if g.Repo != "cli/cli" {
		t.Errorf("repo = %q, want cli/cli", g.Repo)
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
	if coc.Path != "https://github.com/cli/cli/blob/trunk/.github/CODE-OF-CONDUCT.md" {
		t.Errorf("code-of-conduct path = %q, want the reported url", coc.Path)
	}
	contact := guidelinesDocOf(t, g, guidelinesKindContactLinks)
	wantContact := "Ask a question on how to use GitHub CLI — For general-purpose questions and answers, see the Discussions section. (https://github.com/cli/cli/discussions)\n" +
		"Ask a question about the GitHub API — Please check out the GitHub community forum for discussions about the GitHub API. (https://github.community/c/github-ecosystem/37)\n"
	if contact.Body != wantContact {
		t.Errorf("contact-links body = %q, want %q", contact.Body, wantContact)
	}
}

func TestGuidelinesIssueTemplateBodies(t *testing.T) {
	root, _ := setupGuidelines(t, guidelinesPopulated)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "be nice\n")

	names := guidelinesDocOf(t, runGuidelinesJSON(t), guidelinesKindIssueTemplate)
	wantNames := "🐛 Bug report — Report a bug or unexpected behavior while using GitHub CLI\n" +
		"🎨 Submit a design proposal — Submit a design to resolve an open issue that has both `needs-design` and `help-wanted` labels\n" +
		"⭐ Submit a request — Surface a feature or problem that you think should be solved\n"
	if names.Body != wantNames {
		t.Errorf("issue-template body = %q, want the names only %q", names.Body, wantNames)
	}
	if names.Path != guidelinesIssueTemplateDir {
		t.Errorf("issue-template path = %q, want %q", names.Path, guidelinesIssueTemplateDir)
	}

	full := guidelinesDocOf(t, runGuidelinesJSON(t, "--full"), guidelinesKindIssueTemplate)
	if !strings.Contains(full.Body, "### Describe the bug") {
		t.Errorf("--full body = %q, want the template body", full.Body)
	}
}

func TestGuidelinesHumanOutput(t *testing.T) {
	view := decodeGuidelinesRepoView(t, "guidelines-repo-view-populated")
	templateTokens := len(view.PullRequestTemplates[0].Body) / guidelinesCharsPerToken
	root, _ := setupGuidelines(t, guidelinesPopulated)
	writeGuidelinesFile(t, root, "CONTRIBUTING.md", "We follow Conventional Commits.\n")

	out, _, err := runGuidelinesCmd(t)
	if err != nil {
		t.Fatalf("guidelines: %v", err)
	}
	for _, want := range []string{
		"# guidelines cli/cli · 5 documents · fetched now\n",
		fmt.Sprintf("## pr-template PULL_REQUEST_TEMPLATE.md (github · %d tokens)\n", templateTokens),
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

func mustGuidelinesCacheDir(t *testing.T, root string) string {
	t.Helper()
	dir, err := guidelinesCacheDir(root)
	if err != nil {
		t.Fatalf("guidelinesCacheDir(%q): %v", root, err)
	}
	return dir
}

func TestGuidelinesCacheDirKeysTheRepository(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("wt"))
	unrelated := vcstest.Repo(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	mainDir := mustGuidelinesCacheDir(t, f.Dir)

	tests := []struct {
		name     string
		root     string
		wantSame bool
	}{
		{"the main checkout", f.Dir, true},
		{"a linked worktree", f.WorktreePath("wt"), true},
		{"an unrelated repository", unrelated.Dir, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := mustGuidelinesCacheDir(t, tt.root)
			repoPath, err := vcs.RepoCachePath(tt.root)
			if err != nil {
				t.Fatalf("RepoCachePath(%q): %v", tt.root, err)
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
	t.Parallel()
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
			t.Parallel()
			got := guidelinesSignals([]guidelinesDoc{{Kind: guidelinesKindContributing, raw: tt.body}})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("guidelinesSignals(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
