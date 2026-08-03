package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// ghGoldenDir holds the gh runs internal/cli recorded from the real binary, one
// JSON container per scenario. It resolves once, before a fixture chdirs out of
// the package directory.
var ghGoldenDir = func() string {
	dir, err := filepath.Abs(filepath.Join("..", "cli", "testdata", "gh", "cli"))
	if err != nil {
		panic("resolve gh golden dir: " + err.Error())
	}
	return dir
}()

// ghGolden is one recorded gh run: the argv it was given and the streams and
// status it answered with, byte for byte.
type ghGolden struct {
	Argv   []string `json:"argv"`
	Exit   int      `json:"exit"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
}

func loadGHGolden(t *testing.T, name string) ghGolden {
	t.Helper()
	path := filepath.Join(ghGoldenDir, name+".json")
	data, err := os.ReadFile(path) //nolint:gosec // path is the checked-in golden corpus, named by a test literal
	if err != nil {
		t.Fatalf("read gh golden %s: %v", name, err)
	}
	var g ghGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("decode gh golden %s: %v", name, err)
	}
	return g
}

// ghArgvDeviation is the one argument a scenario could not be captured with:
// recorded is what the capture had to pass, production what the real call passes
// in its place — empty when production passes nothing there at all.
type ghArgvDeviation struct{ recorded, production string }

// ghArgvDeviations names every golden whose recorded argv cannot equal the
// invocation production makes, so ghAssertServed still holds a served golden
// against every other argument of the call it answers. testdata/gh/README.md,
// "Where a recording deviates from production argv", carries the reasons: the
// two foreign-repository views must name their repository, because production's
// bare gh repo view reads the working directory's and neither of those is it;
// and viewer-graphql asks for one organization where production asks for a
// hundred, because the recorded account's memberships are mostly private and a
// verbatim capture would publish them in a public repository.
var ghArgvDeviations = map[string]ghArgvDeviation{
	"repo-view-foreign": {recorded: "cli/cli"},
	"repo-view-missing": {recorded: "yasyf/cc-context-does-not-exist"},
	"viewer-graphql": {
		recorded:   "query={viewer{login organizations(first:1){nodes{login}}}}",
		production: "query={viewer{login organizations(first:100){nodes{login}}}}",
	},
}

// ghWantArgv is the invocation a golden may honestly answer: the argv it was
// recorded with, carrying whatever deviation it declares.
func ghWantArgv(t *testing.T, name string) []string {
	t.Helper()
	argv := slices.Clone(loadGHGolden(t, name).Argv)
	dev, ok := ghArgvDeviations[name]
	if !ok {
		return argv
	}
	i := slices.Index(argv, dev.recorded)
	if i < 0 {
		t.Fatalf("golden %s no longer holds the recorded %q — its ghArgvDeviations entry is stale", name, dev.recorded)
	}
	if dev.production == "" {
		return slices.Delete(argv, i, i+1)
	}
	argv[i] = dev.production
	return argv
}

// ghAssertServed holds every call the replay answered against the golden that
// answered it. The script keys a response on the "$1 $2" pair alone, so without
// this a golden recorded for one invocation is served for another and
// production asking GitHub a different question passes unnoticed.
func ghAssertServed(t *testing.T, log string, runs map[string][]string) {
	t.Helper()
	served := map[string]int{}
	for i, argv := range ghCalls(t, log) {
		verb := argv[0] + " " + argv[1]
		served[verb]++
		names := runs[verb]
		if served[verb] > len(names) {
			t.Errorf("gh call %d %q: the recorded %q runs are exhausted", i+1, argv, verb)
			continue
		}
		name := names[served[verb]-1]
		if want := ghWantArgv(t, name); !slices.Equal(argv, want) {
			t.Errorf("gh call %d argv = %q, but golden %s answers %q", i+1, argv, name, want)
		}
	}
}

// ghReplay installs a gh that answers the Nth call for a verb — the "$1 $2" pair
// keying runs — with the Nth golden named for it, and records every argv it saw
// as an argc-prefixed NUL-framed record. GitHub is a network boundary, so the
// binary stays a script; every byte it prints came off a real gh run, a call
// past the recorded runs exits 2 rather than inventing an answer, and every call
// is held against its golden's own argv when the test ends.
func ghReplay(t *testing.T, runs map[string][]string) (argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	state := filepath.Join(dir, "state")
	for verb, names := range runs {
		slug := strings.ReplaceAll(verb, " ", "-")
		mustMkdir(t, state)
		mustWriteFile(t, filepath.Join(state, slug+".n"), "0")
		for i, name := range names {
			g := loadGHGolden(t, name)
			run := filepath.Join(state, slug, strconv.Itoa(i+1))
			mustMkdir(t, run)
			mustWriteFile(t, filepath.Join(run, "stdout"), g.Stdout)
			mustWriteFile(t, filepath.Join(run, "stderr"), g.Stderr)
			mustWriteFile(t, filepath.Join(run, "exit"), strconv.Itoa(g.Exit))
		}
	}

	script := "#!/bin/sh\n" +
		`printf '%s\0' "$#" "$@" >> ` + shQuote(argvLog) + "\n" +
		`state=` + shQuote(state) + "\n" +
		`counter="$state/$1-$2.n"` + "\n" +
		`if [ ! -f "$counter" ]; then` + "\n" +
		`  printf 'gh replay: no recorded runs for: %s\n' "$*" >&2` + "\n" +
		"  exit 2\n" +
		"fi\n" +
		`n=$(($(cat "$counter") + 1))` + "\n" +
		`printf '%s' "$n" > "$counter"` + "\n" +
		`run="$state/$1-$2/$n"` + "\n" +
		`if [ ! -d "$run" ]; then` + "\n" +
		`  printf 'gh replay: call %s of "%s %s" exhausts the recorded runs\n' "$n" "$1" "$2" >&2` + "\n" +
		"  exit 2\n" +
		"fi\n" +
		`cat "$run/stdout"` + "\n" +
		`cat "$run/stderr" >&2` + "\n" +
		`exit "$(cat "$run/exit")"` + "\n"

	binDir := filepath.Join(dir, "bin")
	mustMkdir(t, binDir)
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o700); err != nil { //nolint:gosec // the replay must be owner-executable to serve as a PATH entry
		t.Fatalf("write gh replay: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { ghAssertServed(t, argvLog, runs) })
	return argvLog
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ghCalls decodes the replay's log into one argv per invocation, in order. A log
// never written reads as no calls.
func ghCalls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log) //nolint:gosec // log is the replay's own path, minted under the test's TempDir
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read gh argv log: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != 0 {
		t.Fatalf("gh argv log %s: final record missing its trailing NUL", log)
	}
	fields := strings.Split(string(data[:len(data)-1]), "\x00")
	var out [][]string
	for i := 0; i < len(fields); {
		argc, err := strconv.Atoi(fields[i])
		if err != nil {
			t.Fatalf("gh argv log %s: argc %q at field %d: %v", log, fields[i], i, err)
		}
		if i+1+argc > len(fields) {
			t.Fatalf("gh argv log %s: argc %d at field %d overruns the log", log, argc, i)
		}
		out = append(out, slices.Clone(fields[i+1:i+1+argc]))
		i += 1 + argc
	}
	return out
}

// ghVerbCount counts the recorded calls whose leading argument pair is verb.
func ghVerbCount(t *testing.T, log, verb string) int {
	t.Helper()
	n := 0
	for _, argv := range ghCalls(t, log) {
		if argv[0]+" "+argv[1] == verb {
			n++
		}
	}
	return n
}

// withoutFetchTime drops the timestamp so a record read back off the cache
// compares equal to the one that wrote it: the fetched value carries a monotonic
// reading the JSON round trip does not.
func withoutFetchTime(r Repo) Repo {
	r.FetchedAt = time.Time{}
	return r
}

func TestRepoOwnership(t *testing.T) {
	tests := []struct {
		name                     string
		repo                     Repo
		writable, mine, personal bool
	}{
		{
			name:     "admin on own public repo",
			repo:     Repo{Owner: "yasyf", ViewerLogin: "yasyf", ViewerPermission: "ADMIN", Affiliated: true},
			writable: true, mine: true, personal: true,
		},
		{
			name:     "maintainer on an org repo",
			repo:     Repo{Owner: "poetic", ViewerLogin: "yasyf", ViewerPermission: "MAINTAIN", Affiliated: true},
			writable: true, mine: true,
		},
		{
			name: "write collaborator on a foreign public repo",
			repo: Repo{Owner: "cli", ViewerLogin: "yasyf", ViewerPermission: "WRITE"},
		},
		{
			name: "read on a foreign public repo",
			repo: Repo{Owner: "cli", ViewerLogin: "yasyf", ViewerPermission: "READ"},
		},
		{
			name: "read on a private repo",
			repo: Repo{Owner: "acme", ViewerLogin: "yasyf", ViewerPermission: "READ", IsPrivate: true},
			mine: true,
		},
		{
			name: "zero value is nobody's",
			repo: Repo{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.repo.Writable(); got != tt.writable {
				t.Errorf("Writable() = %v, want %v", got, tt.writable)
			}
			if got := tt.repo.Mine(); got != tt.mine {
				t.Errorf("Mine() = %v, want %v", got, tt.mine)
			}
			if got := tt.repo.Personal(); got != tt.personal {
				t.Errorf("Personal() = %v, want %v", got, tt.personal)
			}
		})
	}
}

// TestViewerAffiliation runs the ownership predicates against the login and
// organizations gh really answered with rather than a literal Affiliated. GitHub
// spells each account in the case its owner chose and compares them without one,
// so the recorded PostPushr membership must still affiliate an owner spelled
// postpushr, and the recorded yasyf an owner spelled YASYF.
func TestViewerAffiliation(t *testing.T) {
	f := vcstest.Repo(t)
	ghReplay(t, map[string][]string{"api graphql": {"viewer-graphql"}})

	v, err := lookupViewer(context.Background(), f.Dir, false)
	if err != nil {
		t.Fatalf("lookupViewer: %v", err)
	}
	if v.Login != "yasyf" || !slices.Equal(v.Orgs, []string{"PostPushr"}) {
		t.Fatalf("viewer = %+v, want the login and organizations the golden recorded", v)
	}

	tests := []struct {
		name                       string
		owner                      string
		affiliated, mine, personal bool
	}{
		{name: "the viewer itself", owner: "yasyf", affiliated: true, mine: true, personal: true},
		{name: "the viewer in another case", owner: "YASYF", affiliated: true, mine: true, personal: true},
		{name: "an organization the viewer belongs to", owner: "PostPushr", affiliated: true, mine: true},
		{name: "that organization in another case", owner: "postpushr", affiliated: true, mine: true},
		{name: "an owner the viewer has nothing to do with", owner: "cli"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := Repo{Owner: tt.owner, ViewerLogin: v.Login, ViewerPermission: "READ"}
			repo.Affiliated = affiliated(repo.Owner, v)
			if repo.Affiliated != tt.affiliated {
				t.Errorf("Affiliated = %v, want %v", repo.Affiliated, tt.affiliated)
			}
			if got := repo.Mine(); got != tt.mine {
				t.Errorf("Mine() = %v, want %v", got, tt.mine)
			}
			if got := repo.Personal(); got != tt.personal {
				t.Errorf("Personal() = %v, want %v", got, tt.personal)
			}
		})
	}
}

// TestLookupRepoCaches walks one repository through every reason a record is or
// is not refetched, with the replay serving a different recorded repository on
// each fetch: which repository comes back is the observable, so a cache that
// stopped caching or a stale record that got served fails on the answer itself
// rather than on a call count.
func TestLookupRepoCaches(t *testing.T) {
	f := vcstest.Repo(t)
	log := ghReplay(t, map[string][]string{
		"repo view":   {"repo-view-own", "repo-view-foreign", "repo-view-own"},
		"api graphql": {"viewer-graphql", "viewer-graphql"},
	})
	ctx := context.Background()

	own := Repo{
		NameWithOwner:    "yasyf/cc-context",
		Owner:            "yasyf",
		ViewerLogin:      "yasyf",
		ViewerPermission: "ADMIN",
		Affiliated:       true,
	}
	foreign := Repo{
		NameWithOwner:    "cli/cli",
		Owner:            "cli",
		ViewerLogin:      "yasyf",
		ViewerPermission: "READ",
	}

	cold, err := LookupRepo(ctx, f.Dir, false)
	if err != nil {
		t.Fatalf("cold LookupRepo: %v", err)
	}
	if got := withoutFetchTime(cold); got != own {
		t.Fatalf("cold LookupRepo = %+v, want %+v", got, own)
	}
	if cold.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero, want the moment of the fetch")
	}
	if !cold.Writable() || !cold.Mine() || !cold.Personal() {
		t.Errorf("own repo: Writable() = %v, Mine() = %v, Personal() = %v, want all true", cold.Writable(), cold.Mine(), cold.Personal())
	}

	warm, err := LookupRepo(ctx, f.Dir, false)
	if err != nil {
		t.Fatalf("warm LookupRepo: %v", err)
	}
	if got := withoutFetchTime(warm); got != own {
		t.Fatalf("warm LookupRepo = %+v, want the cached %+v — a second fetch would have answered cli/cli", got, own)
	}

	refreshed, err := LookupRepo(ctx, f.Dir, true)
	if err != nil {
		t.Fatalf("refreshed LookupRepo: %v", err)
	}
	if got := withoutFetchTime(refreshed); got != foreign {
		t.Fatalf("refreshed LookupRepo = %+v, want the second recorded run %+v", got, foreign)
	}
	if refreshed.Writable() || refreshed.Mine() || refreshed.Personal() {
		t.Errorf("foreign repo: Writable() = %v, Mine() = %v, Personal() = %v, want all false", refreshed.Writable(), refreshed.Mine(), refreshed.Personal())
	}

	bumpRepoSchema(t, f.Dir)
	bumped, err := LookupRepo(ctx, f.Dir, false)
	if err != nil {
		t.Fatalf("post-schema-bump LookupRepo: %v", err)
	}
	if got := withoutFetchTime(bumped); got != own {
		t.Fatalf("post-schema-bump LookupRepo = %+v, want the third recorded run %+v — a record of an unknown schema was served", got, own)
	}
	if got := ghVerbCount(t, log, "api graphql"); got != 2 {
		t.Errorf("gh api graphql calls = %d, want 2: the viewer is cached machine-wide and only the explicit refresh refetches it", got)
	}
}

// TestLookupRepoSharesOneRecordAcrossWorktrees pins the cache key to the
// repository rather than the checkout: the replay holds one recorded run per
// verb, so a linked worktree paying its own lookup exits 2.
func TestLookupRepoSharesOneRecordAcrossWorktrees(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("feat"))
	log := ghReplay(t, map[string][]string{
		"repo view":   {"repo-view-own"},
		"api graphql": {"viewer-graphql"},
	})
	ctx := context.Background()

	main, err := LookupRepo(ctx, f.Dir, false)
	if err != nil {
		t.Fatalf("LookupRepo(main): %v", err)
	}
	linked, err := LookupRepo(ctx, f.WorktreePath("feat"), false)
	if err != nil {
		t.Fatalf("LookupRepo(worktree): %v", err)
	}
	if withoutFetchTime(linked) != withoutFetchTime(main) {
		t.Errorf("worktree lookup = %+v, want the main checkout's %+v", linked, main)
	}
	if got := ghVerbCount(t, log, "repo view"); got != 1 {
		t.Errorf("gh repo view calls = %d, want 1 for both checkouts of one repository", got)
	}
}

// TestLookupRepoUnresolvableName drives the path gh takes when the repository
// does not resolve: an unknowable answer wraps ErrNoGitHub and carries GitHub's
// own diagnostic, and the viewer is never asked, so the replay records none.
func TestLookupRepoUnresolvableName(t *testing.T) {
	f := vcstest.Repo(t)
	log := ghReplay(t, map[string][]string{"repo view": {"repo-view-missing"}})

	repo, err := LookupRepo(context.Background(), f.Dir, false)
	if !errors.Is(err, ErrNoGitHub) {
		t.Fatalf("LookupRepo = %+v, %v; want an error wrapping ErrNoGitHub", repo, err)
	}
	if want := "Could not resolve to a Repository"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to carry gh's own %q", err, want)
	}
	if got := ghVerbCount(t, log, "api graphql"); got != 0 {
		t.Errorf("gh api graphql calls = %d, want 0: a repository that does not resolve needs no viewer", got)
	}
}

func TestLookupRepoWithoutGH(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	if _, err := LookupRepo(context.Background(), t.TempDir(), false); !errors.Is(err, ErrNoGitHub) {
		t.Fatalf("error = %v, want it to wrap ErrNoGitHub", err)
	}
}

// bumpRepoSchema rewrites root's cached record with an unknown schema, the
// shape a future format change leaves behind.
func bumpRepoSchema(t *testing.T, root string) {
	t.Helper()
	path, err := RepoCachePath(root)
	if err != nil {
		t.Fatalf("repo cache path: %v", err)
	}
	var rec repoRecord
	data, err := os.ReadFile(path) //nolint:gosec // path is the test's own cache dir
	if err != nil {
		t.Fatalf("read cached record: %v", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode cached record: %v", err)
	}
	rec.Schema = githubSchema + 1
	bumped, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("encode bumped record: %v", err)
	}
	if err := os.WriteFile(path, bumped, 0o600); err != nil {
		t.Fatalf("write bumped record: %v", err)
	}
}
