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
const gtSchema = 1

// gtReachableTTL and gtUnreachableTTL bound how long a cached reachability
// verdict is served. They are deliberately asymmetric: a stale positive fails
// loudly at gt submit, while a stale negative silently costs the gt lane, and
// someone who has just run gt init should not wait a day for it back.
const (
	gtReachableTTL   = 24 * time.Hour
	gtUnreachableTTL = time.Hour
)

// gtProbeTimeout caps the reachability probe. Offline, gt auth prints its error
// and then hangs indefinitely; when it does answer it takes ~1.5s, so this is
// roughly 3x headroom.
//
// The cap binds gt itself, not its descendants: the deadline kills gt, but any
// grandchild it forked still holds the inherited stdout pipe, and the read
// blocks until that pipe closes, so a gt that forks could outlast this
// deadline. exec.Cmd.WaitDelay fixes that. It is deferred rather than
// overlooked: it belongs in render.RunCLIExitCodeDir, where it changes
// semantics for every RunCLIExitCode caller, so it needs its own enumeration of
// them instead of riding along here.
const gtProbeTimeout = 5 * time.Second

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

// lane is the resolved backend a mutating VCS command runs on.
type lane struct {
	kind vcs.Kind
	root string
	gt   bool
	// note explains a graphite lane that was available but declined — for the
	// report line and ccx vcs info's lane_reason.
	note string
	// repo and reachable are the cached inputs the gates weighed, carried so a
	// caller reporting the lane quotes the same verdicts it turned on instead of
	// asking gh and gt a second time. Both are nil when the gates short-circuited
	// before asking.
	repo      *vcs.Repo
	reachable *bool
}

// gtRecord is one repo's cached gt-reachability verdict, stored beside its
// GitHub metadata on the same TTL.
type gtRecord struct {
	Schema    int       `json:"schema"`
	Reachable bool      `json:"reachable"`
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
func resolveLaneRefresh(ctx context.Context, name, dir string, noGT, refresh bool) (lane, error) {
	kind, root := vcs.DetectRoot(dir)
	if kind == vcs.None {
		return lane{}, fmt.Errorf("%s: no git or jj repository in the working directory", name)
	}
	l := lane{kind: kind, root: root}
	if noGT || !vcs.GraphiteRepo(ctx, root) {
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

	reachable, why, err := gtReachability(ctx, root, refresh)
	if err != nil {
		return lane{}, fmt.Errorf("%s: %w", name, err)
	}
	l.reachable = &reachable
	if !reachable {
		l.note = why
		return l, nil
	}
	l.gt = true
	return l, nil
}

// kindLabel names a lane's backend for the report.
func kindLabel(kind vcs.Kind) string {
	switch kind {
	case vcs.JJ:
		return "jj"
	case vcs.Git:
		return "git"
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
// when one is on disk so the ~1.5s probe runs at most once a day per repo.
// An unknown answer is neither cached nor demoting.
func gtReachability(ctx context.Context, root string, refresh bool) (bool, string, error) {
	path, err := gtCachePath(root)
	if err != nil {
		return false, "", err
	}
	if !refresh {
		if rec, ok := readGTRecord(path); ok {
			return rec.Reachable, rec.Note, nil
		}
	}

	var reachable bool
	var note string
	err = cache.WithLock(ctx, filepath.Dir(path), "gt", func() error {
		// Re-read under the lock: a concurrent probe of the same repo has likely
		// already paid for the answer this one was about to ask for.
		if !refresh {
			if rec, ok := readGTRecord(path); ok {
				reachable, note = rec.Reachable, rec.Note
				return nil
			}
		}
		probed, why, known := gtReachable(ctx, root)
		if !known {
			reachable = true
			return nil
		}
		data, err := json.Marshal(gtRecord{Schema: gtSchema, Reachable: probed, Note: why, FetchedAt: time.Now()})
		if err != nil {
			return fmt.Errorf("marshal gt reachability for %q: %w", root, err)
		}
		reachable, note = probed, why
		return cache.Store(path, data, 0o600)
	})
	if err != nil {
		return false, "", err
	}
	return reachable, note, nil
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
func gtReachable(ctx context.Context, root string) (reachable bool, note string, known bool) {
	probeCtx, cancel := context.WithTimeout(ctx, gtProbeTimeout)
	defer cancel()

	// -q suppresses the ready line while keeping the exit code, so the predicate
	// needs the unquiet form.
	stdout, code, stderr, err := render.RunCLIExitCodeDir(probeCtx, root, "gt", []string{"auth", "--no-interactive"})
	// A killed process surfaces as a signal exit, not an error, so the deadline
	// is read off the context rather than the exit code.
	if err != nil || probeCtx.Err() != nil {
		return false, "", false
	}
	return classifyGTProbe(stdout+stderr, code)
}

// classifyGTProbe maps the probe's combined output and exit code to a verdict.
// The lane survives only when we could not ask: an unreachable server, or an
// exit 0 that never confirms submittability. Any nonzero exit demotes,
// recognized or not — gt was asked and declined.
func classifyGTProbe(output string, code int) (reachable bool, note string, known bool) {
	switch {
	case code == 0 && strings.Contains(output, gtProbeReady):
		return true, "", true
	case code == 0, strings.Contains(output, gtProbeUnreadable):
		return false, "", false
	case strings.Contains(output, gtProbeNoPerms):
		return false, gtProbeLine(output, gtProbeNoPerms), true
	case strings.Contains(output, gtProbeNoToken):
		return false, "graphite has no auth token — run gt auth --token <token>", true
	default:
		return false, gtProbeFallbackNote(output), true
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

// gtCachePath resolves the cached reachability verdict for root, a sibling of
// its GitHub metadata record.
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
	if rec.Reachable {
		ttl = gtReachableTTL
	}
	if time.Since(rec.FetchedAt) >= ttl {
		return gtRecord{}, false
	}
	return rec, true
}
