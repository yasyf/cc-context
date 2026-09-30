// Package prstate keeps one machine-wide view of a repository's pull requests.
// Whichever process asks first polls GitHub for every pull request any process
// on the machine still watches, in one batched query, and the rest read the
// cache file it leaves, so every watcher spends one shared GitHub budget.
package prstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
)

const (
	// MinInterval is the least time between two polls of one repository by any
	// process on the machine; a read of data younger than this polls nothing.
	MinInterval = 30 * time.Second
	// ProbeEvery caps the wait between cheap probes while GitHub refuses the
	// machine's requests, however far out the reset it advertised.
	ProbeEvery = 2 * time.Minute

	// QueueLabel is the label that puts a pull request in Graphite's queue.
	QueueLabel = "merge"
	// QueueLabelFast is the label that puts it in the queue's fast lane.
	QueueLabelFast = "merge-fast"
	// ActivityHeading opens the merge activity comment Graphite's queue edits in
	// place on each pull request it handles.
	ActivityHeading = "### Merge activity"

	leaseTTL         = 15 * time.Minute
	keepFor          = 24 * time.Hour
	defaultLimitWait = time.Minute
	rateFloor        = 100
	stateFile        = "state.json"
)

// Want names what one reader needs fresh: pull requests by number, and the
// open pull requests on branches under each prefix.
type Want struct {
	PRs      []int
	Prefixes []string
}

// State is a repository's shared view as of its last poll.
type State struct {
	PolledAt time.Time       `json:"polledAt"`
	Trunk    Trunk           `json:"trunk"`
	Lanes    map[string]Lane `json:"lanes,omitempty"`
	PRs      map[int]PR      `json:"prs,omitempty"`
	Rate     Rate            `json:"rate"`
	Backoff  *Backoff        `json:"backoff,omitempty"`
	Leases   Leases          `json:"leases"`
}

// PR is one pull request as the last poll that asked for it read it.
type PR struct {
	Number           int                    `json:"number"`
	State            string                 `json:"state"`
	Title            string                 `json:"title"`
	Author           string                 `json:"author,omitempty"`
	CreatedAt        time.Time              `json:"createdAt"`
	BaseRefName      string                 `json:"baseRefName"`
	HeadRefName      string                 `json:"headRefName"`
	HeadRefOid       string                 `json:"headRefOid"`
	Mergeable        string                 `json:"mergeable"`
	MergeStateStatus string                 `json:"mergeStateStatus"`
	ReviewDecision   string                 `json:"reviewDecision"`
	ChangedFiles     int                    `json:"changedFiles"`
	MergeCommit      string                 `json:"mergeCommit,omitempty"`
	Labels           []string               `json:"labels,omitempty"`
	Status           string                 `json:"status,omitempty"`
	Rollup           *Rollup                `json:"rollup,omitempty"`
	Activity         string                 `json:"activity,omitempty"`
	Graphite         *gtapi.PullRequestInfo `json:"graphite,omitempty"`
	SquashOn         []string               `json:"squashOn,omitempty"`
	PolledAt         time.Time              `json:"polledAt"`
}

// Rollup is the head commit's aggregate check state and the contexts behind
// it, in GitHub's GraphQL shape.
type Rollup struct {
	State    string `json:"state"`
	Contexts struct {
		Nodes []Context `json:"nodes"`
	} `json:"contexts"`
}

// Context is one CheckRun or StatusContext of a Rollup.
type Context struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Status     string `json:"status,omitempty"`
	Context    string `json:"context,omitempty"`
	State      string `json:"state,omitempty"`
}

// Trunk is the default branch and its newest hundred commits.
type Trunk struct {
	Name    string   `json:"name"`
	History []Commit `json:"history,omitempty"`
}

// Commit is one trunk commit's id and subject.
type Commit struct {
	OID      string `json:"oid"`
	Headline string `json:"headline"`
}

// Lane is the open pull requests on branches under one prefix.
type Lane struct {
	PRs      []int     `json:"prs"`
	PolledAt time.Time `json:"polledAt"`
}

// Rate is GitHub's GraphQL quota as the last poll read it.
type Rate struct {
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

// Backoff is GitHub refusing the machine's requests: nothing polls before
// ProbeAt, when one cheap request tests whether the refusal lifted.
type Backoff struct {
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	ProbeAt time.Time `json:"probeAt"`
	Reason  string    `json:"reason"`
}

// Leases is when a reader last asked for each pull request and prefix; a
// poll reads everything asked for within leaseTTL.
type Leases struct {
	PRs      map[int]time.Time    `json:"prs,omitempty"`
	Prefixes map[string]time.Time `json:"prefixes,omitempty"`
}

// LimitedError is a read the budget refused: GitHub is limiting the machine,
// and no request goes out before ProbeAt.
type LimitedError struct {
	Backoff
}

func (e *LimitedError) Error() string {
	return fmt.Sprintf("github %s since %s; next probe at %s",
		e.Reason, e.Since.UTC().Format(time.RFC3339), e.ProbeAt.UTC().Format(time.RFC3339))
}

// MissingError names pull requests the repository does not have.
type MissingError struct {
	Repo string
	PRs  []int
}

func (e *MissingError) Error() string {
	refs := make([]string, 0, len(e.PRs))
	for _, n := range e.PRs {
		refs = append(refs, fmt.Sprintf("#%d", n))
	}
	return fmt.Sprintf("%s has no pull request %s", e.Repo, strings.Join(refs, " "))
}

// Store is one repository's shared view on disk and the source that polls it.
type Store struct {
	dir   string
	src   *GitHub
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// DefaultRoot is the machine-wide directory every Store lives under. It
// ignores $CLAUDE_PLUGIN_DATA so plugin and shell processes share one budget.
func DefaultRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("prstate: resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "cc-context", "prstate"), nil
}

// Open returns the Store for src's repository under root.
func Open(root string, src *GitHub) (*Store, error) {
	dir := filepath.Join(root, src.owner, src.name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("prstate: create %s: %w", dir, err)
	}
	return &Store{dir: dir, src: src, now: time.Now, sleep: sleep}, nil
}

// Read returns the shared view with everything want names at most MinInterval
// old, polling first when it is not. Every read renews want's leases, so the
// next poll by any process reads them too. A read the budget refuses returns
// the stale view with a *LimitedError.
func (s *Store) Read(ctx context.Context, want Want) (State, error) {
	var st State
	err := cache.WithLock(ctx, s.dir, "poll", func() error {
		var err error
		st, err = s.read(ctx, want)
		return err
	})
	return st, err
}

func (s *Store) read(ctx context.Context, want Want) (State, error) {
	st, err := s.load()
	if err != nil {
		return State{}, err
	}
	now := s.now()
	st.Leases.renew(want, now)
	if st.fresh(want, now) {
		return st, s.save(st)
	}
	if b := st.Backoff; b != nil {
		if now.Before(b.ProbeAt) {
			return st, s.refuse(st)
		}
		rate, err := s.src.probe(ctx)
		if wait, limited := ghapi.RateLimited(err); limited {
			st.Backoff = b.again(now, wait)
			return st, s.refuse(st)
		}
		if err != nil {
			return st, err
		}
		st.Backoff, st.Rate = nil, rate
	}
	if wait := st.PolledAt.Add(MinInterval).Sub(now); wait > 0 {
		if err := s.sleep(ctx, wait); err != nil {
			return st, err
		}
		now = s.now()
	}
	if err := s.pollInto(ctx, &st, st.request(now), now); err != nil {
		return st, err
	}
	if unread := st.unread(want, now); len(unread) > 0 {
		if err := s.pollInto(ctx, &st, Want{PRs: unread}, now); err != nil {
			return st, err
		}
	}
	if err := s.save(st); err != nil {
		return st, err
	}
	if missing := slices.DeleteFunc(slices.Clone(want.PRs), func(n int) bool { _, ok := st.PRs[n]; return ok }); len(missing) > 0 {
		return st, &MissingError{Repo: s.src.owner + "/" + s.src.name, PRs: missing}
	}
	return st, nil
}

func (s *Store) pollInto(ctx context.Context, st *State, req Want, now time.Time) error {
	poll, err := s.src.poll(ctx, req, st.PRs)
	if wait, limited := ghapi.RateLimited(err); limited {
		st.Backoff = limitedAt(now, wait)
		return s.refuse(*st)
	}
	if err != nil {
		return err
	}
	st.absorb(poll, now)
	return nil
}

func (s *Store) refuse(st State) error {
	if err := s.save(st); err != nil {
		return err
	}
	return &LimitedError{Backoff: *st.Backoff}
}

func (s *Store) load() (State, error) {
	path := filepath.Join(s.dir, stateFile)
	data, err := os.ReadFile(path) //nolint:gosec // under the machine's own cache dir
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("prstate: read %s: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("prstate: parse %s: %w", path, err)
	}
	return st, nil
}

func (s *Store) save(st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("prstate: encode state: %w", err)
	}
	if err := cache.Store(filepath.Join(s.dir, stateFile), data, 0o600); err != nil {
		return fmt.Errorf("prstate: write state: %w", err)
	}
	return nil
}

func (l *Leases) renew(want Want, now time.Time) {
	if l.PRs == nil {
		l.PRs = map[int]time.Time{}
	}
	if l.Prefixes == nil {
		l.Prefixes = map[string]time.Time{}
	}
	for _, n := range want.PRs {
		l.PRs[n] = now
	}
	for _, prefix := range want.Prefixes {
		l.Prefixes[prefix] = now
	}
}

func (st State) fresh(want Want, now time.Time) bool {
	for _, n := range want.PRs {
		pr, ok := st.PRs[n]
		if !ok || !pr.settled() && now.Sub(pr.PolledAt) >= MinInterval {
			return false
		}
	}
	for _, prefix := range want.Prefixes {
		lane, ok := st.Lanes[prefix]
		if !ok || now.Sub(lane.PolledAt) >= MinInterval {
			return false
		}
	}
	return true
}

func (st State) unread(want Want, now time.Time) []int {
	var unread []int
	for _, prefix := range want.Prefixes {
		for _, n := range st.Lanes[prefix].PRs {
			if pr := st.PRs[n]; pr.PolledAt.Before(now) && !pr.settled() && !slices.Contains(unread, n) {
				unread = append(unread, n)
			}
		}
	}
	slices.Sort(unread)
	return unread
}

func (st State) request(now time.Time) Want {
	var req Want
	for n, at := range st.Leases.PRs {
		if now.Sub(at) < leaseTTL && !st.PRs[n].settled() {
			req.PRs = append(req.PRs, n)
		}
	}
	for prefix, at := range st.Leases.Prefixes {
		if now.Sub(at) < leaseTTL {
			req.Prefixes = append(req.Prefixes, prefix)
		}
	}
	slices.Sort(req.PRs)
	slices.Sort(req.Prefixes)
	return req
}

func (st *State) absorb(p poll, now time.Time) {
	st.PolledAt, st.Trunk, st.Rate = now, p.trunk, p.rate
	if st.PRs == nil {
		st.PRs = map[int]PR{}
	}
	if st.Lanes == nil {
		st.Lanes = map[string]Lane{}
	}
	for n, pr := range p.prs {
		pr.PolledAt = now
		st.PRs[n] = pr
	}
	for prefix, prs := range p.lanes {
		st.Lanes[prefix] = Lane{PRs: prs, PolledAt: now}
		for _, n := range prs {
			st.Leases.PRs[n] = now
		}
	}
	for _, n := range p.missing {
		delete(st.Leases.PRs, n)
		delete(st.PRs, n)
	}
	for n, at := range st.Leases.PRs {
		if now.Sub(at) >= leaseTTL {
			delete(st.Leases.PRs, n)
		}
	}
	for prefix, at := range st.Leases.Prefixes {
		if now.Sub(at) >= leaseTTL {
			delete(st.Leases.Prefixes, prefix)
			delete(st.Lanes, prefix)
		}
	}
	for n, pr := range st.PRs {
		if _, leased := st.Leases.PRs[n]; !leased && now.Sub(pr.PolledAt) >= keepFor {
			delete(st.PRs, n)
		}
	}
	if p.rate.Remaining < rateFloor && p.rate.ResetAt.After(now) {
		st.Backoff = &Backoff{Since: now, Until: p.rate.ResetAt, ProbeAt: p.rate.ResetAt, Reason: "quota below the floor"}
	}
}

func (pr PR) settled() bool {
	return pr.State != "" && pr.State != "OPEN" && len(pr.SquashOn) > 0
}

// QueueLabelled reports whether pr carries a label Graphite's queue watches.
func (pr PR) QueueLabelled() bool {
	return slices.Contains(pr.Labels, QueueLabel) || slices.Contains(pr.Labels, QueueLabelFast)
}

func limitedAt(now time.Time, wait time.Duration) *Backoff {
	return (&Backoff{Since: now}).again(now, wait)
}

func (b *Backoff) again(now time.Time, wait time.Duration) *Backoff {
	if wait <= 0 {
		wait = defaultLimitWait
	}
	return &Backoff{Since: b.Since, Until: now.Add(wait), ProbeAt: now.Add(min(wait, ProbeEvery)), Reason: "rate limit"}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
