package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// gtSchema is the on-disk format version of the cached reachability verdict;
// a mismatch reads as a miss.
const gtSchema = 2

// gtReachableTTL through gtUnknownTTL bound how long a cached reachability
// verdict is served. They are deliberately asymmetric: a stale positive fails
// loudly at gt submit, while a stale negative silently costs the gt lane, and
// someone who has just run gt init should not wait a day for it back. Unknown
// is shortest by an order of magnitude — it is the one verdict nobody could
// confirm, so it is worth re-asking as soon as the answer might have changed.
const (
	gtReachableTTL   = 24 * time.Hour
	gtUnreachableTTL = time.Hour
	gtUnknownTTL     = time.Minute
)

// gtProbeTimeout caps the reachability probe. Offline, gt auth prints its error
// and then hangs indefinitely, so some cap is required.
//
// Measured over 36 runs across 6 repos and both verdicts, an answering probe
// took 5.47s at the fastest, 6.67s median, 12.85s at the slowest, with a
// separate reading at 17.9s. The previous 5s cap sat below every one of those
// observations — it was derived from a single ~1.5s sample and timed out every
// probe in the field, which made this whole gate inert. Re-derive this from the
// distribution, never from one run. A var so tests that exercise the deadline
// can shorten it.
var gtProbeTimeout = 20 * time.Second

// gtProbeReady through gtProbeUnreadable are gt 1.8.6's own wording for
// classifyGTProbe; version-dependent, kept as lone constants so an upgrade is a
// one-line change. Only the first two are load-bearing — an unrecognized
// failure demotes anyway, so the rest only buy a better note.
const (
	gtProbeReady      = "Ready to submit PRs to"
	gtProbeNoPerms    = "does not have the necessary permissions to submit PRs to"
	gtProbeNoToken    = "No auth token set" //nolint:gosec // G101 false positive: gt's own stdout wording, matched against — it holds no credential
	gtProbeUnreadable = "Could not connect to the Graphite server"
)

// gtProbeNoteBudget caps an unrecognized failure's note, so a verbose gt error
// cannot flood the report's one-line lane segment.
const gtProbeNoteBudget = 200

// nogtKey is the git-config key that durably opts a repo out of the graphite
// lane, so a repo carrying a stale Graphite config stops re-litigating it on
// every ship.
const nogtKey = "ccx.nogt"

// gtVerdict answers whether Graphite can submit for a repo. Unknown is its own
// answer, not a synonym for either other: only gtVerdictOK keeps the gt lane,
// so a probe nobody could get an answer out of demotes rather than riding on an
// assumption it never verified.
type gtVerdict string

const (
	gtVerdictOK      gtVerdict = "yes"
	gtVerdictDenied  gtVerdict = "no"
	gtVerdictUnknown gtVerdict = "unknown"
)

// lane is the resolved backend a mutating VCS command runs on.
type lane struct {
	kind     vcs.Kind
	root     string
	checkout vcs.Checkout
	broken   *vcs.BrokenCheckout
	gt       bool
	// note explains a graphite lane that was available but declined — for the
	// report line and ccx vcs info's lane_reason.
	note string
	// repo and verdict are the cached inputs the gates weighed, carried so a
	// caller reporting the lane quotes the same verdicts it turned on instead of
	// asking gh and gt a second time. Both are zero when the gates
	// short-circuited before asking.
	repo    *vcs.Repo
	verdict gtVerdict
}

// gtRecord is one repo's cached gt-reachability verdict, stored beside its
// GitHub metadata. Every verdict is written, each on its own TTL.
type gtRecord struct {
	Schema    int       `json:"schema"`
	Verdict   gtVerdict `json:"verdict"`
	Note      string    `json:"note"`
	FetchedAt time.Time `json:"fetched_at"`
}

// resolveLane detects the working copy under dir and applies the graphite gates,
// all before any mutation, weighing the gates' inputs from cache. name prefixes
// every returned error, matching each command's existing style.
func resolveLane(ctx context.Context, name, dir string, noGT bool) (lane, error) {
	return resolveLaneRefresh(ctx, name, dir, noGT, false)
}

// resolveLaneRefresh is resolveLane with refresh refetching the inputs the gates
// weigh — the repository's GitHub metadata and gt's reachability verdict — so a
// caller that reports the lane can re-probe the verdict it turns on rather than
// only the line describing it.
//
// A broken checkout is the one error returned alongside a populated lane: the
// backend and the working copy are known even when the repository behind them
// is not, and resolveLaneReport hands that diagnosis on rather than exiting. The
// lane takes them off the partial checkout ResolveCheckout returns with the
// error, so the root it reports is the canonical one the diagnosis names — the
// two lines of that report are one path spelled one way.
func resolveLaneRefresh(ctx context.Context, name, dir string, noGT, refresh bool) (lane, error) {
	ck, err := vcs.ResolveCheckout(dir)
	var broken *vcs.BrokenCheckout
	if errors.As(err, &broken) {
		return lane{kind: ck.Kind, root: ck.Root, broken: broken}, fmt.Errorf("%s: %w", name, err)
	}
	if err != nil {
		return lane{}, fmt.Errorf("%s: %w", name, err)
	}
	if ck.Kind == vcs.None {
		return lane{}, fmt.Errorf("%s: no git or jj repository in the working directory", name)
	}
	root := ck.Root
	l := lane{kind: ck.Kind, root: root, checkout: ck}
	if noGT {
		return l, nil
	}
	graphite, err := vcs.GraphiteRepo(ck)
	if err != nil {
		return lane{}, fmt.Errorf("%s: %w", name, err)
	}
	if !graphite {
		return l, nil
	}
	if gtDisabled(ctx, root) {
		l.note = "gt disabled for this repo (" + nogtKey + ")"
		return l, nil
	}
	if _, err := exec.LookPath("gt"); err != nil {
		return lane{}, fmt.Errorf("%s: graphite config found but gt not on PATH — install graphite (brew install graphite) or pass --no-gt", name)
	}

	repo, err := vcs.LookupRepo(ctx, root, refresh)
	switch {
	case errors.Is(err, vcs.ErrNoGitHub):
		// Unknown is not "not yours": the gate only demotes on a positive answer.
	case err != nil:
		return lane{}, fmt.Errorf("%s: %w", name, err)
	default:
		l.repo = &repo
		if !repo.Mine() {
			l.note = fmt.Sprintf("%s is public and you have %s access", repo.NameWithOwner, strings.ToLower(repo.ViewerPermission))
			return l, nil
		}
	}

	verdict, why, err := gtReachability(ctx, root, refresh)
	if err != nil {
		return lane{}, fmt.Errorf("%s: %w", name, err)
	}
	l.verdict = verdict
	if verdict != gtVerdictOK {
		l.note = why
		return l, nil
	}
	l.gt = true
	return l, nil
}

// resolveLaneReport resolves the lane for a reporting command, carrying a broken
// checkout on the lane instead of refusing: a command whose whole output is a
// diagnosis of the working copy is the one caller that must survive one.
func resolveLaneReport(ctx context.Context, name, dir string, noGT, refresh bool) (lane, error) {
	l, err := resolveLaneRefresh(ctx, name, dir, noGT, refresh)
	var broken *vcs.BrokenCheckout
	if errors.As(err, &broken) {
		return l, nil
	}
	return l, err
}

// kindLabel names a lane's backend for the report.
func kindLabel(kind vcs.Kind) string {
	switch kind {
	case vcs.JJ:
		return "jj"
	case vcs.Git:
		return "git"
	case vcs.None:
		return "none"
	default:
		panic(fmt.Sprintf("cli: no lane label for vcs kind %d", kind))
	}
}

// gtDisabled reports whether root opts out of the graphite lane. An unset key
// exits nonzero, which is the answer "not disabled", not a failure.
func gtDisabled(ctx context.Context, root string) bool {
	out, err := render.RunCLIDir(ctx, root, "git", []string{"config", "--get", nogtKey})
	if err != nil {
		return false
	}
	disabled, err := strconv.ParseBool(strings.TrimSpace(out))
	return err == nil && disabled
}

// gtReachability reports whether gt can drive root, serving a cached verdict
// when one is on disk so a reachable repo pays for the probe at most once a day.
// An unknown answer is cached too, briefly. Unknown demotes, so a cached unknown
// is a cached demotion — it cannot resurrect the gt lane it just declined, which
// is what makes storing it safe. Not storing it would leave every command during
// an outage paying a fresh 20s probe to re-derive the same answer.
func gtReachability(ctx context.Context, root string, refresh bool) (gtVerdict, string, error) {
	path, err := gtCachePath(root)
	if err != nil {
		return "", "", err
	}
	if !refresh {
		if rec, ok := readGTRecord(path); ok {
			return rec.Verdict, rec.Note, nil
		}
	}

	var verdict gtVerdict
	var note string
	err = cache.WithLock(ctx, filepath.Dir(path), "gt", func() error {
		// Re-read under the lock: a concurrent probe of the same repo has likely
		// already paid for the answer this one was about to ask for.
		if !refresh {
			if rec, ok := readGTRecord(path); ok {
				verdict, note = rec.Verdict, rec.Note
				return nil
			}
		}
		verdict, note = gtReachable(ctx, root)
		data, err := json.Marshal(gtRecord{Schema: gtSchema, Verdict: verdict, Note: note, FetchedAt: time.Now()})
		if err != nil {
			return fmt.Errorf("marshal gt reachability for %q: %w", root, err)
		}
		return cache.Store(path, data, 0o600)
	})
	if err != nil {
		return "", "", err
	}
	return verdict, note, nil
}

// gtReachable asks Graphite whether root is submittable, reporting the verdict
// and whether the answer is known at all. Only the server can answer: a live
// .graphite_repo_config carries no repo identity — it holds trunk names and
// fetch timestamps, identical across every repo that has one — so no local file
// can stand in for this round trip.
//
// Bare gt auth is an undocumented status check. It rewrites
// .git/.graphite_pr_info, a small PR cache, and touches nothing else — notably
// not .graphite_repo_config, so the probe cannot flip lane detection on.
func gtReachable(ctx context.Context, root string) (gtVerdict, string) {
	probeCtx, cancel := context.WithTimeout(ctx, gtProbeTimeout)
	defer cancel()

	// -q suppresses the ready line while keeping the exit code, so the predicate
	// needs the unquiet form.
	stdout, code, stderr, err := render.RunCLIProbeDir(probeCtx, root, "gt", []string{"auth", "--no-interactive"})
	output := stdout + stderr
	if err != nil {
		return gtVerdictUnknown, "gt auth could not run: " + err.Error()
	}
	// A killed process surfaces as a signal exit, not an error, so the deadline
	// is read off the context rather than the exit code.
	if probeCtx.Err() != nil {
		return gtVerdictUnknown, gtProbeAbortNote(output)
	}
	return classifyGTProbe(output, code)
}

// gtProbeAbortNote reads a reason off a probe that never returned an exit code.
// Offline, gt prints its error and only then hangs, so the output it managed to
// write before the deadline names a cause the deadline alone cannot.
func gtProbeAbortNote(output string) string {
	if strings.Contains(output, gtProbeUnreadable) {
		return "graphite server unreachable"
	}
	return "gt auth did not answer within " + gtProbeTimeout.String()
}

// classifyGTProbe maps the probe's combined output and exit code to a verdict.
// Only gt's own ready line is a yes: an exit 0 that never confirms
// submittability is unknown, not consent. Any nonzero exit is a no, recognized
// or not — gt was asked and declined.
func classifyGTProbe(output string, code int) (gtVerdict, string) {
	switch {
	case code == 0 && strings.Contains(output, gtProbeReady):
		return gtVerdictOK, ""
	case strings.Contains(output, gtProbeUnreadable):
		return gtVerdictUnknown, "graphite server unreachable"
	case code == 0:
		return gtVerdictUnknown, "gt auth exited 0 without confirming this repo is submittable"
	case strings.Contains(output, gtProbeNoPerms):
		return gtVerdictDenied, gtProbeLine(output, gtProbeNoPerms)
	case strings.Contains(output, gtProbeNoToken):
		return gtVerdictDenied, "graphite has no auth token — run gt auth --token <token>"
	default:
		return gtVerdictDenied, gtProbeFallbackNote(output)
	}
}

// gtProbeFallbackNote summarizes an unrecognized failure as gt's first non-empty
// output line, capped.
func gtProbeFallbackNote(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > gtProbeNoteBudget {
			return strings.ToValidUTF8(trimmed[:gtProbeNoteBudget], "") + "…"
		}
		return trimmed
	}
	return "gt auth failed without output"
}

// gtProbeLine returns the whole output line carrying marker, so the note quotes
// gt's own wording — which names the repo — rather than paraphrasing it.
func gtProbeLine(output, marker string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, marker) {
			return strings.TrimSpace(line)
		}
	}
	return marker
}

// gtCachePath resolves the cached reachability verdict for the repository root
// belongs to, a sibling of its GitHub metadata record. The key is the
// repository, not the checkout, so a repository's linked worktrees share one
// verdict and the single probe behind it.
func gtCachePath(root string) (string, error) {
	repoPath, err := vcs.RepoCachePath(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(repoPath), "gt.json"), nil
}

func readGTRecord(path string) (gtRecord, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // path is rooted at the cache dir and keyed by sha256 hex
	if err != nil {
		return gtRecord{}, false
	}
	var rec gtRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Schema != gtSchema {
		return gtRecord{}, false
	}
	ttl := gtUnreachableTTL
	switch rec.Verdict {
	case gtVerdictOK:
		ttl = gtReachableTTL
	case gtVerdictUnknown:
		ttl = gtUnknownTTL
	}
	if time.Since(rec.FetchedAt) >= ttl {
		return gtRecord{}, false
	}
	return rec, true
}
