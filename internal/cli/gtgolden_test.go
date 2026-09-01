package cli

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gtGoldenDir holds one JSON file per recorded gt invocation plus a sibling
// .md, written by scripts/record-gt-goldens.sh. Its README.md describes the
// layout.
const gtGoldenDir = "testdata/gt"

// gtGolden is one recorded gt run, read back exactly as gt wrote it.
type gtGolden struct {
	name   string
	argv   []string
	stdout string
	stderr string
	exit   int
}

// result rebuilds the run a classifier is handed. The two streams are joined
// the way gtCapture joins them, which is the one join ccx makes over streams it
// kept apart; gtRun's own interleaving is not reconstructible from two files,
// and no classifier reads anything finer than a line.
func (g gtGolden) result() gtResult {
	return gtResult{Output: gtJoinStreams(g.stdout, g.stderr), Stderr: g.stderr, Code: g.exit}
}

func loadGTGolden(t *testing.T, name string) gtGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(gtGoldenDir, name+".json"))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	var rec struct {
		Argv   []string `json:"argv"`
		Stdout string   `json:"stdout"`
		Stderr string   `json:"stderr"`
		Exit   int      `json:"exit"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return gtGolden{
		name:   name,
		argv:   rec.Argv,
		stdout: rec.Stdout,
		stderr: rec.Stderr,
		exit:   rec.Exit,
	}
}

// gtFamily is the production classifier a recorded verb reaches.
type gtFamily int

const (
	// gtFamilyProbe is gtReachable's gt auth, classified by classifyGTProbe.
	gtFamilyProbe gtFamily = iota
	// gtFamilyRestack is gtSync's gt sync, classified by classifyGTRestack
	// under gtZeroSurfaces. A recorded gt restack belongs here too: sync
	// restacks, and the sentences ccx matches are the restacker's — see the
	// scenario READMEs.
	gtFamilyRestack
	// gtFamilySubmit is shipPushGT's gt submit, classified by classifyGTSubmit
	// under gtZeroFatal.
	gtFamilySubmit
)

func gtGoldenFamily(t *testing.T, g gtGolden) gtFamily {
	t.Helper()
	switch g.argv[0] {
	case "auth":
		return gtFamilyProbe
	case "sync", "restack":
		return gtFamilyRestack
	case "submit":
		return gtFamilySubmit
	default:
		t.Fatalf("golden %s: no classifier owns gt %s", g.name, g.argv[0])
		return 0
	}
}

// gtGoldenCase is what one recorded scenario must classify to. Every recorded
// scenario has one and every case names a recorded scenario, both enforced by
// TestGTGoldenWalk, so a golden can neither arrive nor vanish without a verdict.
type gtGoldenCase struct {
	// diagnostics is how many lines of the recorded stderr lead with a severity
	// prefix — nonzero is what opens Diagnostics' gate, which then reports that
	// stderr whole — and reported whether any of them was an ERROR: rather than
	// a WARNING:.
	diagnostics int
	reported    bool
	// wantErr is whether the verb's own gtZeroPolicy calls this run a failure.
	wantErr bool
	// advice is the recovery step the classifier replaces gt's sentence with.
	// Empty means gt's wording is none this package recognizes, and the failure
	// must be wrapped verbatim — the arm a reword falls into.
	advice string
	// verdict and note are classifyGTProbe's answer. note is ccx's own sentence;
	// quotes instead names the marker whose whole line gt supplies as the note.
	verdict gtVerdict
	note    string
	quotes  string
	// skipped is what gtSyncSkipped reads out of the output, and skippedPath the
	// same for a reason ending in the recorder's own work root, matched by its
	// lead because that path moves with the machine that recorded it.
	skipped     map[string]string
	skippedPath map[string]string
}

var gtGoldenCases = map[string]gtGoldenCase{
	"restack-conflict": {
		wantErr: true,
		advice:  "restack: conflict — resolve the listed files, then gt continue (or gt abort); see the output above",
	},
	"restack-blocked-during-rebase": {
		diagnostics: 1,
		reported:    true,
		wantErr:     true,
	},
	"restack-worktree-held": {
		skippedPath: map[string]string{"feat": "checked out in "},
	},
	// The --cwd counterpart of restack-worktree-held: the same held branch, and
	// gt restacks it because the run happens inside the working copy holding it.
	// Nothing is declined, which is the point — a lane sweep's per-lane run is
	// the one that moves the branch every other run reports as untouchable.
	"restack-cwd-held": {},
	"restack-frozen": {
		skipped: map[string]string{"feat": "frozen"},
	},
	// The five exit-0 syncs the echo exists for. None is a failure: gt did
	// fast-forward the trunk, and restack's verdict re-measures the stack
	// itself, so a WARNING: here explains a report ccx already made. The two
	// with no severity line are the gate's negative case — tips are unprefixed
	// stderr, so without the gate an ordinary sync would report them.
	"sync-decline-exit0": {
		diagnostics: 1,
	},
	"sync-decline-unstaged-exit0": {
		diagnostics: 2,
	},
	"sync-tips-exit0":  {},
	"sync-quiet-exit0": {},
	"sync-tips-and-warning-exit0": {
		diagnostics: 1,
	},
	"sync-no-remote": {
		diagnostics: 1,
		reported:    true,
		wantErr:     true,
	},
	"sync-auth-invalid": {
		diagnostics: 1,
		reported:    true,
		wantErr:     true,
		advice:      "restack: graphite auth required — run gt auth",
	},
	"sync-repo-404": {
		diagnostics: 1,
		reported:    true,
		wantErr:     true,
	},
	"submit-unauth": {
		diagnostics: 1,
		reported:    true,
		wantErr:     true,
		advice:      "ship: graphite auth required — run gt auth",
	},
	"submit-repo-unverified": {
		diagnostics: 2,
		reported:    true,
		wantErr:     true,
	},
	"auth-no-token": {
		diagnostics: 1,
		reported:    true,
		verdict:     gtVerdictDenied,
		note:        "graphite has no auth token — run gt auth --token <token>",
	},
	"auth-no-perms": {
		diagnostics: 1,
		reported:    true,
		verdict:     gtVerdictDenied,
		quotes:      gtProbeNoPerms,
	},
	"auth-unreachable": {
		diagnostics: 1,
		reported:    true,
		verdict:     gtVerdictUnknown,
		note:        "graphite server unreachable",
	},
	"auth-authenticated-elsewhere": {
		verdict: gtVerdictUnknown,
		note:    "gt auth exited 0 without confirming this repo is submittable",
	},
}

// assertGTGolden drives the pure classifiers over one recorded run: no process,
// no repo, only the bytes gt wrote.
func assertGTGolden(t *testing.T, g gtGolden, want gtGoldenCase) {
	t.Helper()
	r := g.result()

	severity := 0
	for _, line := range strings.Split(r.Stderr, "\n") {
		if strings.HasPrefix(line, gtErrorPrefix) || strings.HasPrefix(line, gtWarningPrefix) {
			severity++
		}
	}
	if severity != want.diagnostics {
		t.Errorf("recorded stderr leads %d line(s) with a severity prefix, want %d", severity, want.diagnostics)
	}
	report := r.Diagnostics()
	if want.diagnostics == 0 {
		if report != "" {
			t.Errorf("Diagnostics() = %q, want %q", report, "")
		}
	} else {
		if !strings.HasSuffix(report, "\n") {
			t.Errorf("Diagnostics() = %q, want a trailing newline", report)
		}
		if strings.Contains(report, gtTipPrefix) {
			t.Errorf("Diagnostics() = %q, want gt's NUX tips dropped", report)
		}
		for _, line := range strings.Split(r.Stderr, "\n") {
			if gtSeverityLed(line) && !strings.Contains(report, line) {
				t.Errorf("Diagnostics() dropped the recorded %q", line)
			}
		}
	}
	if got := r.reportedError(); got != want.reported {
		t.Errorf("reportedError() = %v, want %v", got, want.reported)
	}

	switch gtGoldenFamily(t, g) {
	case gtFamilyProbe:
		verdict, note := classifyGTProbe(r.Output, r.Code)
		if verdict != want.verdict {
			t.Errorf("classifyGTProbe() verdict = %q, want %q", verdict, want.verdict)
		}
		switch {
		case want.quotes != "":
			if !strings.Contains(note, want.quotes) {
				t.Errorf("classifyGTProbe() note = %q, want gt's own line carrying %q", note, want.quotes)
			}
			if !slices.Contains(strings.Split(r.Output, "\n"), note) {
				t.Errorf("classifyGTProbe() note = %q, which is no whole line of the recorded output", note)
			}
		case note != want.note:
			t.Errorf("classifyGTProbe() note = %q, want %q", note, want.note)
		}
	case gtFamilyRestack:
		err := r.verdict("sync", gtZeroSurfaces)
		assertGTGoldenAdvice(t, r, err, want, classifyGTRestack, "restack: ")
		assertGTGoldenSkipped(t, r, want)
	case gtFamilySubmit:
		err := r.verdict("submit", gtZeroFatal)
		assertGTGoldenAdvice(t, r, err, want, classifyGTSubmit, "ship: ")
	}
}

// assertGTGoldenAdvice checks the policy verdict, then the recovery step the
// classifier reads off the run. A case naming no advice must reach the arm that
// wraps gt's failure verbatim, and gt's own error must stay reachable through
// it either way.
func assertGTGoldenAdvice(t *testing.T, r gtResult, err error, want gtGoldenCase, classify func(gtResult, error) error, prefix string) {
	t.Helper()
	if (err != nil) != want.wantErr {
		t.Fatalf("verdict() = %v, want an error: %v", err, want.wantErr)
	}
	if err == nil {
		return
	}
	got := classify(r, err)
	if !errors.Is(got, err) {
		t.Errorf("classified error %v does not reach gt's own failure %v", got, err)
	}
	if want.advice != "" {
		if got.Error() != want.advice {
			t.Errorf("classified error = %q, want %q — gt likely reworded the sentence this arm matches", got.Error(), want.advice)
		}
		return
	}
	if got.Error() != prefix+err.Error() {
		t.Errorf("classified error = %q, want gt's failure wrapped verbatim as %q", got.Error(), prefix+err.Error())
	}
}

func assertGTGoldenSkipped(t *testing.T, r gtResult, want gtGoldenCase) {
	t.Helper()
	got := gtSyncSkipped(r.Output)
	branches := slices.Sorted(maps.Keys(got))
	wantBranches := slices.Concat(slices.Collect(maps.Keys(want.skipped)), slices.Collect(maps.Keys(want.skippedPath)))
	slices.Sort(wantBranches)
	if !slices.Equal(branches, wantBranches) {
		t.Fatalf("gtSyncSkipped() named %q, want %q", branches, wantBranches)
	}
	for branch, reason := range want.skipped {
		if got[branch] != reason {
			t.Errorf("gtSyncSkipped()[%q] = %q, want %q", branch, got[branch], reason)
		}
	}
	for branch, lead := range want.skippedPath {
		reason := got[branch]
		if !strings.HasPrefix(reason, lead) {
			t.Errorf("gtSyncSkipped()[%q] = %q, want it to lead with %q", branch, reason, lead)
		}
		if path := strings.TrimPrefix(reason, lead); !strings.HasPrefix(path, "/") || strings.HasSuffix(path, ".") {
			t.Errorf("gtSyncSkipped()[%q] = %q, want an absolute path with gt's sentence-ending period dropped", branch, reason)
		}
	}
}

// TestGTGoldenClassifiers drives every pure gt classifier over the recorded
// bytes: Diagnostics, reportedError, verdict, gtSyncSkipped, classifyGTRestack,
// classifyGTSubmit and classifyGTProbe, none of which runs a process.
func TestGTGoldenClassifiers(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(gtGoldenCases)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertGTGolden(t, loadGTGolden(t, name), gtGoldenCases[name])
		})
	}
}

// gtGoldenFellThrough reports whether the production classifier no longer
// recognizes any of the scenario's wording, and can do nothing with the run but
// hand gt's own failure back. It is what a reworded gt looks like from here.
func gtGoldenFellThrough(t *testing.T, g gtGolden) bool {
	t.Helper()
	r := g.result()
	switch gtGoldenFamily(t, g) {
	case gtFamilyProbe:
		verdict, note := classifyGTProbe(r.Output, r.Code)
		return verdict == gtVerdictDenied && note == gtProbeFallbackNote(r.Output)
	case gtFamilySubmit:
		err := r.verdict("submit", gtZeroFatal)
		return err != nil && classifyGTSubmit(r, err).Error() == "ship: "+err.Error()
	default:
		err := r.verdict("sync", gtZeroSurfaces)
		return err != nil && classifyGTRestack(r, err).Error() == "restack: "+err.Error()
	}
}

// TestGTGoldenWalk is the alarm for a gt that rewords itself. It walks the
// corpus rather than the case table, so a re-recording cannot quietly add or
// drop a scenario: every recorded scenario must carry a case, every case must
// name a recorded scenario, and every recorded run must still reach the arm its
// case says it does rather than falling through to gt's own words. Every
// scenario — recorded or not — must say in its sibling .md what it holds.
func TestGTGoldenWalk(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(gtGoldenDir)
	if err != nil {
		t.Fatalf("read %s: %v", gtGoldenDir, err)
	}
	if version, err := os.ReadFile(filepath.Join(gtGoldenDir, "VERSION")); err != nil || strings.TrimSpace(string(version)) == "" {
		t.Fatalf("VERSION = %q, %v — the corpus must name the gt it came from", version, err)
	}

	recorded := map[string]bool{}
	scenarios := map[string]bool{}
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
			scenarios[name] = true
			recorded[name] = true
			continue
		}
		if name, ok := strings.CutSuffix(entry.Name(), ".md"); ok && name != "README" {
			scenarios[name] = true
		}
	}

	for _, name := range slices.Sorted(maps.Keys(scenarios)) {
		t.Run(name, func(t *testing.T) {
			readme, err := os.ReadFile(filepath.Join(gtGoldenDir, name+".md"))
			if err != nil || len(readme) == 0 {
				t.Fatalf("%s.md = %q, %v — every scenario says what it holds", name, readme, err)
			}
			if !recorded[name] {
				if !strings.Contains(string(readme), "NOT RECORDED") {
					t.Errorf("%s holds no bytes, so its .md must say NOT RECORDED and why", name)
				}
				return
			}
			want, ok := gtGoldenCases[name]
			if !ok {
				t.Fatalf("%s is recorded but no gtGoldenCase says what it classifies to", name)
			}
			g := loadGTGolden(t, name)
			if len(g.argv) == 0 || g.argv[0] == "" {
				t.Fatalf("%s argv = %q, want the verb gt was given", name, g.argv)
			}
			if got, wantFell := gtGoldenFellThrough(t, g), want.wantErr && want.advice == ""; got != wantFell {
				t.Errorf("%s falls through to gt's own words = %v, want %v — gt reworded what this scenario pins", name, got, wantFell)
			}
		})
	}

	for _, name := range slices.Sorted(maps.Keys(gtGoldenCases)) {
		if !recorded[name] {
			t.Errorf("gtGoldenCase %q names no recorded scenario — re-record it or drop the case", name)
		}
	}
}

// gtGoldenStream is what this repo's commit hooks would rewrite in one recorded
// stream: lines ending in whitespace, which trailing-whitespace strips, and the
// blank line a tip block ends on, which end-of-file-fixer collapses.
type gtGoldenStream struct {
	trailingWS int
	blankTail  bool
}

// gtGoldenUnnormalized declares every recorded stream holding such a byte,
// addressed as scenario.field. gt's splog.error template ends its line with a
// space before the newline, and its tip block ends on a blank line; both are
// evidence of what gt wrote, and both are what these goldens exist to hold. As
// a JSON string a payload occupies no end of line and no end of file, so neither
// hook can reach one. A hook that reached one anyway, or a hand-normalized
// golden, drops its entry here; a re-recording that produces a new one must add
// its entry.
var gtGoldenUnnormalized = map[string]gtGoldenStream{
	"auth-no-perms.stderr":                 {trailingWS: 1},
	"auth-no-token.stderr":                 {trailingWS: 1},
	"auth-unreachable.stderr":              {trailingWS: 1},
	"restack-blocked-during-rebase.stderr": {trailingWS: 1},
	"restack-frozen.stderr":                {blankTail: true},
	"restack-cwd-held.stderr":              {blankTail: true},
	"restack-worktree-held.stderr":         {blankTail: true},
	"submit-repo-unverified.stderr":        {trailingWS: 1},
	"submit-unauth.stderr":                 {trailingWS: 1},
	"sync-auth-invalid.stderr":             {trailingWS: 1},
	"sync-no-remote.stderr":                {trailingWS: 1},
	"sync-repo-404.stderr":                 {trailingWS: 1},
	// The exit-0 syncs. Their stdout ends on the blank line below gt's
	// "Restacking branches..." banner — the very blankness that makes the
	// stderr echo the only account of a decline — and the tip-bearing ones end
	// stderr on a tip block's own closing blank line.
	"sync-decline-exit0.stdout":          {blankTail: true},
	"sync-decline-unstaged-exit0.stdout": {blankTail: true},
	"sync-tips-and-warning-exit0.stderr": {blankTail: true},
	"sync-tips-and-warning-exit0.stdout": {blankTail: true},
	"sync-tips-exit0.stderr":             {blankTail: true},
}

// gtGoldenShape reads what the hooks would rewrite out of one payload.
func gtGoldenShape(payload string) (gtGoldenStream, bool) {
	shape := gtGoldenStream{blankTail: strings.HasSuffix(payload, "\n\n")}
	for _, line := range strings.Split(payload, "\n") {
		if strings.TrimRight(line, " \t\r") != line {
			shape.trailingWS++
		}
	}
	return shape, shape != (gtGoldenStream{})
}

// TestGTGoldenPayloadsAreUnnormalized is the alarm for a golden that stopped
// being what gt wrote. It sweeps every recorded stream for the bytes this repo's
// commit hooks rewrite and holds the result against gtGoldenUnnormalized, so a
// payload that got normalized fails by name, as loudly as one that arrived
// undeclared.
func TestGTGoldenPayloadsAreUnnormalized(t *testing.T) {
	t.Parallel()
	got := map[string]gtGoldenStream{}
	for _, name := range slices.Sorted(maps.Keys(gtGoldenCases)) {
		g := loadGTGolden(t, name)
		for _, field := range []string{"stdout", "stderr"} {
			payload := g.stdout
			if field == "stderr" {
				payload = g.stderr
			}
			if shape, hostile := gtGoldenShape(payload); hostile {
				got[name+"."+field] = shape
			}
		}
	}

	for _, addr := range slices.Sorted(maps.Keys(gtGoldenUnnormalized)) {
		switch shape, ok := got[addr]; {
		case !ok:
			t.Errorf("%s no longer carries %+v — a hook or a hand edit normalized what gt wrote", addr, gtGoldenUnnormalized[addr])
		case shape != gtGoldenUnnormalized[addr]:
			t.Errorf("%s carries %+v, want %+v", addr, shape, gtGoldenUnnormalized[addr])
		}
	}
	for _, addr := range slices.Sorted(maps.Keys(got)) {
		if _, ok := gtGoldenUnnormalized[addr]; !ok {
			t.Errorf("%s carries %+v but is undeclared — record it in gtGoldenUnnormalized so a later normalization fails here", addr, got[addr])
		}
	}
}

// gtGoldenSeverityLines reads the severity-led lines out of one recorded stream.
func gtGoldenSeverityLines(payload string) []string {
	var got []string
	for _, line := range strings.Split(payload, "\n") {
		if strings.HasPrefix(line, gtErrorPrefix) || strings.HasPrefix(line, gtWarningPrefix) {
			got = append(got, line)
		}
	}
	return got
}

// TestGTGoldenSeverityStaysOnStderr pins gt's splog contract over the recorded
// corpus: gt writes every severity-led line to stderr, never to stdout. The
// goldens keep the two streams apart, so a recording — or a rewrite of the
// corpus — that crossed or trimmed them fails here rather than silently handing
// a classifier a line from the wrong stream.
func TestGTGoldenSeverityStaysOnStderr(t *testing.T) {
	t.Parallel()
	onStderr := 0
	for _, name := range slices.Sorted(maps.Keys(gtGoldenCases)) {
		g := loadGTGolden(t, name)
		if got := gtGoldenSeverityLines(g.stdout); len(got) > 0 {
			t.Errorf("%s stdout carries %q — gt writes severity-led lines to stderr, so the recorded streams got crossed", name, got)
		}
		onStderr += len(gtGoldenSeverityLines(g.stderr))
	}
	if onStderr == 0 {
		t.Error("no recorded scenario carries a severity-led line on stderr, so nothing pins the split")
	}
}

// TestGTGoldenSubmitArgvIsShipsOwn pins the submit golden to the argv ship
// actually builds, so the recorded refusal is the answer to ccx's own call
// rather than to a command nobody makes.
func TestGTGoldenSubmitArgvIsShipsOwn(t *testing.T) {
	t.Parallel()
	g := loadGTGolden(t, "submit-unauth")
	if got := gtSubmitArgv(shipOpts{}); !slices.Equal(got, g.argv) {
		t.Errorf("gtSubmitArgv() = %q, want the recorded %q", got, g.argv)
	}
}

// TestGTGoldenJoinStreamsKeepsBothStreamsWhole pins gtJoinStreams against the
// recorded runs that wrote to both streams: gt splits one report across them,
// so a join that glued the streams together would hide a diagnostic behind a
// prefix that no longer starts a line.
func TestGTGoldenJoinStreamsKeepsBothStreamsWhole(t *testing.T) {
	t.Parallel()
	both := 0
	for _, name := range slices.Sorted(maps.Keys(gtGoldenCases)) {
		g := loadGTGolden(t, name)
		if g.stdout == "" || g.stderr == "" {
			continue
		}
		both++
		lines := strings.Split(gtJoinStreams(g.stdout, g.stderr), "\n")
		for _, stream := range []string{g.stdout, g.stderr} {
			for _, line := range strings.Split(strings.TrimSuffix(stream, "\n"), "\n") {
				if !slices.Contains(lines, line) {
					t.Errorf("%s: joined output lost the whole line %q", name, line)
				}
			}
		}
	}
	if both == 0 {
		t.Error("no recorded scenario writes to both streams, so nothing pins gtJoinStreams")
	}
}
