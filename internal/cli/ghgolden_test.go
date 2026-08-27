package cli

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ghGoldenDir holds one JSON file per recorded gh run, written by
// scripts/record-gh-goldens.sh. Its README.md describes the layout: cli/ is what
// a PATH fake replays, api/ what an httptest.Server serves.
const ghGoldenDir = "testdata/gh"

// ghGoldenFields is the exact key set each kind's container carries. Decoding
// ignores a key that is not there, so without this a half-written scenario would
// read back as a zero exit and an empty stream rather than fail.
var ghGoldenFields = map[string][]string{
	"cli": {"argv", "exit", "stdout", "stderr"},
	"api": {"argv", "exit", "status", "headers", "body", "stderr"},
}

// ghGolden is one recorded gh run, read back exactly as gh wrote it.
type ghGolden struct {
	name   string
	argv   []string
	stdout string
	stderr string
	exit   int
}

// ghAPIGolden is one recorded GitHub response, read back exactly as GitHub sent
// it. internal/ghapi speaks HTTP directly, so its goldens are responses rather
// than gh stdout; argv is the gh api -i invocation that captured one, and
// headers holds only the fields a parser reads.
type ghAPIGolden struct {
	name    string
	argv    []string
	status  int
	headers []string
	body    string
	stderr  string
	exit    int
}

func decodeGHGolden(t *testing.T, kind, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ghGoldenDir, kind, name+".json"))
	if err != nil {
		t.Fatalf("golden %s/%s: %v", kind, name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("golden %s/%s: %v", kind, name, err)
	}
}

func loadGHGolden(t *testing.T, name string) ghGolden {
	t.Helper()
	var rec struct {
		Argv   []string `json:"argv"`
		Exit   int      `json:"exit"`
		Stdout string   `json:"stdout"`
		Stderr string   `json:"stderr"`
	}
	decodeGHGolden(t, "cli", name, &rec)
	return ghGolden{name: name, argv: rec.Argv, stdout: rec.Stdout, stderr: rec.Stderr, exit: rec.Exit}
}

func loadGHAPIGolden(t *testing.T, name string) ghAPIGolden {
	t.Helper()
	var rec struct {
		Argv    []string `json:"argv"`
		Exit    int      `json:"exit"`
		Status  int      `json:"status"`
		Headers []string `json:"headers"`
		Body    string   `json:"body"`
		Stderr  string   `json:"stderr"`
	}
	decodeGHGolden(t, "api", name, &rec)
	return ghAPIGolden{name: name, argv: rec.Argv, status: rec.Status, headers: rec.Headers, body: rec.Body, stderr: rec.Stderr, exit: rec.Exit}
}

// payloads is every recorded stream of one scenario, keyed by the field holding
// it, which is what TestGHGoldenPayloadsAreUnnormalized sweeps.
func (g ghGolden) payloads() map[string]string {
	return map[string]string{"stdout": g.stdout, "stderr": g.stderr}
}

func (g ghAPIGolden) payloads() map[string]string {
	return map[string]string{"body": g.body, "stderr": g.stderr}
}

// ghGoldenNames lists one kind's recorded scenarios, and is where a corpus that
// stopped being one container per scenario surfaces — a leftover directory from
// the layout this one replaced reads as a scenario nobody can load.
func ghGoldenNames(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ghGoldenDir, kind))
	if err != nil {
		t.Fatalf("read %s/%s: %v", ghGoldenDir, kind, err)
	}
	names := []string{}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || entry.IsDir() {
			t.Errorf("%s/%s holds %q — every scenario is one .json container", ghGoldenDir, kind, entry.Name())
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatalf("%s/%s holds no scenarios", ghGoldenDir, kind)
	}
	return names
}

// TestGHGoldenWalk walks the corpus rather than a case table, so a re-recording
// cannot half-write a scenario: every container carries its kind's whole key
// set, names the invocation that produced it, and — for the responses
// internal/ghapi replays — reports the status and headers only a gh api -i
// capture could have supplied.
func TestGHGoldenWalk(t *testing.T) {
	t.Parallel()
	if version, err := os.ReadFile(filepath.Join(ghGoldenDir, "VERSION")); err != nil || strings.TrimSpace(string(version)) == "" {
		t.Fatalf("VERSION = %q, %v — the corpus must name the gh it came from", version, err)
	}

	for _, kind := range slices.Sorted(maps.Keys(ghGoldenFields)) {
		for _, name := range ghGoldenNames(t, kind) {
			t.Run(kind+"/"+name, func(t *testing.T) {
				t.Parallel()
				var keys map[string]json.RawMessage
				decodeGHGolden(t, kind, name, &keys)
				if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, slices.Sorted(slices.Values(ghGoldenFields[kind]))) {
					t.Fatalf("holds %q, want %q — a half-written scenario", got, ghGoldenFields[kind])
				}
				if kind == "cli" {
					if argv := loadGHGolden(t, name).argv; len(argv) == 0 || argv[0] == "" {
						t.Errorf("argv = %q, want the arguments gh was given", argv)
					}
					return
				}
				g := loadGHAPIGolden(t, name)
				if !slices.Equal(g.argv[:min(2, len(g.argv))], []string{"api", "-i"}) {
					t.Errorf("argv = %q, want a gh api -i capture — nothing else carries the status and headers", g.argv)
				}
				if g.status < 100 {
					t.Errorf("status = %d, want the HTTP status GitHub answered with", g.status)
				}
				for _, header := range g.headers {
					if _, _, ok := strings.Cut(header, ": "); !ok {
						t.Errorf("header %q is not a Name: value field", header)
					}
				}
			})
		}
	}
}

// ghGoldenPayload is what this repo's commit hooks would rewrite in one recorded
// stream: lines ending in whitespace, which trailing-whitespace strips, and a
// missing final newline, which end-of-file-fixer supplies.
type ghGoldenPayload struct {
	trailingWS     int
	noFinalNewline bool
}

// ghGoldenUnnormalized declares every recorded payload holding such a byte,
// addressed as kind/scenario.field. These are the reason the corpus is JSON
// containers rather than one raw file per stream: as a JSON string a payload
// occupies no end of line and no end of file, so neither hook can reach it.
// A hook that reached one anyway, or a hand-normalized golden, drops its entry
// here; a re-recording that produces a new one must add its entry.
var ghGoldenUnnormalized = map[string]ghGoldenPayload{
	"api/reviews-graphql-branches.body":         {noFinalNewline: true},
	"api/reviews-graphql-missing.body":          {noFinalNewline: true},
	"api/reviews-graphql-numbers.body":          {noFinalNewline: true},
	"api/reviews-inline-comments.body":          {noFinalNewline: true},
	"api/reviews-inline-comments-empty.body":    {noFinalNewline: true},
	"api/reviews-inline-comments-outdated.body": {noFinalNewline: true},
	"api/reviews-inline-comments-since.body":    {noFinalNewline: true},
	"api/reviews-issue-comments.body":           {noFinalNewline: true},
	"api/reviews-paginate-page1.body":           {noFinalNewline: true},
	"api/reviews-paginate-page2.body":           {noFinalNewline: true},
	"api/reviews-paginate-page3.body":           {noFinalNewline: true},
	"api/reviews-pull-missing.body":             {noFinalNewline: true},
	"api/reviews-reviews.body":                  {noFinalNewline: true},
	"cli/downstack-graphql-one.stdout":          {noFinalNewline: true},
	"cli/downstack-graphql-three.stdout":        {noFinalNewline: true},
	"cli/guidelines-profile-found.stdout":       {noFinalNewline: true},
	"cli/guidelines-profile-none.stdout":        {noFinalNewline: true},
	"cli/status-comment-graphql.stdout":         {noFinalNewline: true},
	"cli/status-draft-graphql.stdout":           {noFinalNewline: true},
	"cli/status-graphql-one.stdout":             {noFinalNewline: true},
	"cli/status-graphql-three.stdout":           {noFinalNewline: true},
	"cli/viewer-graphql.stdout":                 {noFinalNewline: true},
	// The one recorded payload carrying trailing whitespace: an Actions log,
	// whose step lines gh pads and whose interleaved progress output ends in a
	// carriage return.
	"cli/run-log-failed.stdout": {trailingWS: 11},
}

// ghGoldenShape reads what the hooks would rewrite out of one payload.
func ghGoldenShape(payload string) (ghGoldenPayload, bool) {
	shape := ghGoldenPayload{noFinalNewline: payload != "" && !strings.HasSuffix(payload, "\n")}
	for _, line := range strings.Split(payload, "\n") {
		if strings.TrimRight(line, " \t\r") != line {
			shape.trailingWS++
		}
	}
	return shape, shape != (ghGoldenPayload{})
}

// TestGHGoldenPayloadsAreUnnormalized is the alarm for a golden that stopped
// being what gh printed. It sweeps every recorded stream for the bytes this
// repo's commit hooks rewrite and holds the result against
// ghGoldenUnnormalized, so a payload that got normalized fails as loudly as one
// that arrived undeclared.
func TestGHGoldenPayloadsAreUnnormalized(t *testing.T) {
	t.Parallel()
	got := map[string]ghGoldenPayload{}
	sweep := func(kind, name string, payloads map[string]string) {
		for _, field := range slices.Sorted(maps.Keys(payloads)) {
			if shape, hostile := ghGoldenShape(payloads[field]); hostile {
				got[kind+"/"+name+"."+field] = shape
			}
		}
	}
	for _, name := range ghGoldenNames(t, "cli") {
		sweep("cli", name, loadGHGolden(t, name).payloads())
	}
	for _, name := range ghGoldenNames(t, "api") {
		sweep("api", name, loadGHAPIGolden(t, name).payloads())
	}

	for _, addr := range slices.Sorted(maps.Keys(ghGoldenUnnormalized)) {
		switch shape, ok := got[addr]; {
		case !ok:
			t.Errorf("%s no longer carries %+v — a hook or a hand edit normalized what gh printed", addr, ghGoldenUnnormalized[addr])
		case shape != ghGoldenUnnormalized[addr]:
			t.Errorf("%s carries %+v, want %+v", addr, shape, ghGoldenUnnormalized[addr])
		}
	}
	for _, addr := range slices.Sorted(maps.Keys(got)) {
		if _, ok := ghGoldenUnnormalized[addr]; !ok {
			t.Errorf("%s carries %+v but is undeclared — record it in ghGoldenUnnormalized so a later normalization fails here", addr, got[addr])
		}
	}
}
