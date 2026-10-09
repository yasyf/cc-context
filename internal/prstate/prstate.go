// Package prstate keeps one machine-wide view of a repository's pull requests.
// Whichever process asks first polls GitHub for every pull request any process
// on the machine still watches, in one batched query, and the rest read the
// cache file it leaves, so every watcher spends one shared GitHub budget.
package prstate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	pushLag          = 10 * time.Minute
	defaultLimitWait = time.Minute
	rateFloor        = 100
	stateFile        = "state.json"
	leaseDir         = "leases"
)

// Want names what one reader needs fresh: pull requests by number, and the
// open pull requests on branches under each prefix.
type Want struct {
	PRs      []int
	Prefixes []string
}

// State is a repository's shared view as of its last poll.
type State struct {
	PolledAt    time.Time       `json:"polledAt"`
	AttemptedAt time.Time       `json:"attemptedAt"`
	Trunk       Trunk           `json:"trunk"`
	Lanes       map[string]Lane `json:"lanes,omitempty"`
	PRs         map[int]PR      `json:"prs,omitempty"`
	Rate        Rate            `json:"rate"`
	Backoff     *Backoff        `json:"backoff,omitempty"`
	Failure     *Failure        `json:"failure,omitempty"`
	Leases      Leases          `json:"-"`
}

// Failure is the error the last poll ended in. A reader that slept out the
// interval returns it rather than poll again before the interval ends.
type Failure struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
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
	Reviews          []Review               `json:"reviews,omitempty"`
	Draft            bool                   `json:"draft,omitempty"`
	ChangedFiles     int                    `json:"changedFiles"`
	MergeCommit      string                 `json:"mergeCommit,omitempty"`
	Labels           []string               `json:"labels,omitempty"`
	Status           string                 `json:"status,omitempty"`
	Rollup           *Rollup                `json:"rollup,omitempty"`
	Activity         string                 `json:"activity,omitempty"`
	Graphite         *gtapi.PullRequestInfo `json:"graphite,omitempty"`
	Mergeability     string                 `json:"mergeability,omitempty"`
	SquashOn         []string               `json:"squashOn,omitempty"`
	PolledAt         time.Time              `json:"polledAt"`
	// PushedHead is the head this machine pushed at PushedAt, kept while
	// GitHub still shows another; HeadRefOid and the checks stay GitHub's.
	PushedHead string    `json:"pushedHead,omitempty"`
	PushedAt   time.Time `json:"pushedAt,omitzero"`
}

// Review is one reviewer's latest approving or change-requesting review.
type Review struct {
	Author string `json:"author"`
	State  string `json:"state"`
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
// poll reads everything asked for within leaseTTL. Each lease is a file whose
// mtime a reader renews without the state lock.
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
	if err := os.MkdirAll(filepath.Join(dir, leaseDir), 0o750); err != nil {
		return nil, fmt.Errorf("prstate: create %s: %w", dir, err)
	}
	return &Store{dir: dir, src: src, now: func() time.Time { return time.Now().UTC() }, sleep: sleep}, nil
}

// Read returns the shared view with everything want names at most MinInterval
// old, polling first when it is not. Every read renews want's leases, so the
// next poll by any process reads them too. A read the budget refuses returns
// the stale view with a *LimitedError.
//
// A read whose only stale records are pull requests no poll has read since
// their last push fetches just those at once, leaving the shared poll's
// schedule alone. Otherwise, inside MinInterval of the last poll a read sleeps,
// so concurrent readers share the next poll. No read holds a lock across a
// request, and a read of fresh records takes none.
func (s *Store) Read(ctx context.Context, want Want) (State, error) {
	if err := s.renew(want, s.now()); err != nil {
		return State{}, err
	}
	for waited := false; ; waited = true {
		st, wait, err := s.read(ctx, want, waited)
		if err != nil || wait <= 0 {
			return st, err
		}
		if err := s.sleep(ctx, wait); err != nil {
			return st, err
		}
	}
}

// Pushed records the head this machine just pushed to each pull request, so
// a read before GitHub shows it answers with the pushed head rather than the
// one polled before the push.
func (s *Store) Pushed(ctx context.Context, heads map[int]string) error {
	now := s.now()
	if err := s.renew(Want{PRs: slices.Collect(maps.Keys(heads))}, now); err != nil {
		return err
	}
	_, err := s.commit(ctx, func(st *State) bool {
		if st.PRs == nil {
			st.PRs = map[int]PR{}
		}
		for n, head := range heads {
			pr := st.PRs[n]
			if pr.PushedAt.After(now) {
				continue
			}
			pr.Number, pr.PushedHead, pr.PushedAt = n, head, now
			st.PRs[n] = pr
		}
		return true
	})
	return err
}

func (s *Store) read(ctx context.Context, want Want, waited bool) (State, time.Duration, error) {
	st, err := s.load()
	if err != nil {
		return State{}, 0, err
	}
	now := s.now()
	if st.fresh(want, now) {
		return st, 0, nil
	}
	if b := st.Backoff; b != nil {
		if now.Before(b.ProbeAt) {
			return st, 0, &LimitedError{Backoff: *b}
		}
		if st, err = s.probe(ctx, st, now); err != nil {
			return st, 0, err
		}
	}
	if prs, ok := st.unpolled(want, now); ok && st.Failure == nil {
		st, err := s.fetch(ctx, Want{PRs: prs}, st.PRs, now, false)
		if err != nil {
			return st, 0, err
		}
		return st, 0, s.verdict(st, want, now)
	}
	if next := st.AttemptedAt.Add(MinInterval); now.Before(next) {
		if f := st.Failure; f != nil && waited {
			return st, 0, fmt.Errorf("the poll at %s failed, and the next goes out at %s: %s",
				f.At.UTC().Format(time.RFC3339), next.UTC().Format(time.RFC3339), f.Reason)
		}
		return st, next.Sub(now), nil
	}
	st, claimed, err := s.claim(ctx, want, now)
	if err != nil {
		return st, 0, err
	}
	if !claimed {
		return s.read(ctx, want, waited)
	}
	req := st.request(now)
	if st, err = s.fetch(ctx, req, st.PRs, now, true); err != nil {
		return st, 0, err
	}
	if unread := st.unread(req.Prefixes, now); len(unread) > 0 && st.Backoff == nil {
		if st, err = s.fetch(ctx, Want{PRs: unread}, st.PRs, now, true); err != nil {
			return st, 0, err
		}
	}
	return st, 0, s.verdict(st, want, now)
}

func (s *Store) verdict(st State, want Want, now time.Time) error {
	if missing := slices.DeleteFunc(slices.Clone(want.PRs), func(n int) bool { _, ok := st.PRs[n]; return ok }); len(missing) > 0 {
		return &MissingError{Repo: s.src.owner + "/" + s.src.name, PRs: missing}
	}
	if st.Backoff != nil && !st.fresh(want, now) {
		return &LimitedError{Backoff: *st.Backoff}
	}
	return nil
}

func (s *Store) probe(ctx context.Context, st State, now time.Time) (State, error) {
	rate, err := s.src.probe(ctx)
	wait, limited := ghapi.RateLimited(err)
	if err != nil && !limited {
		return st, err
	}
	st, err = s.commit(ctx, func(st *State) bool {
		switch {
		case limited && st.Backoff != nil:
			st.Backoff = st.Backoff.again(now, wait)
		case limited:
			st.Backoff = limitedAt(now, wait)
		default:
			st.Backoff, st.Rate = quotaBackoff(rate, now), rate
		}
		return true
	})
	if err == nil && st.Backoff != nil {
		err = &LimitedError{Backoff: *st.Backoff}
	}
	return st, err
}

func (s *Store) claim(ctx context.Context, want Want, now time.Time) (State, bool, error) {
	claimed := false
	st, err := s.commit(ctx, func(st *State) bool {
		if st.fresh(want, now) || st.Backoff != nil || now.Before(st.AttemptedAt.Add(MinInterval)) {
			return false
		}
		st.AttemptedAt, st.Failure, claimed = now, nil, true
		return true
	})
	return st, claimed, err
}

func (s *Store) fetch(ctx context.Context, req Want, prev map[int]PR, now time.Time, attempt bool) (State, error) {
	p, err := s.src.poll(ctx, req, prev)
	wait, limited := ghapi.RateLimited(err)
	st, cerr := s.commit(ctx, func(st *State) bool {
		switch {
		case limited:
			st.Backoff = limitedAt(now, wait)
		case err == nil:
			st.absorb(p, now)
		case attempt && ctx.Err() == nil && st.AttemptedAt.Equal(now):
			st.Failure = &Failure{At: now, Reason: err.Error()}
		default:
			return false
		}
		return true
	})
	if limited {
		err = &LimitedError{Backoff: *limitedAt(now, wait)}
	}
	return st, errors.Join(err, cerr)
}

func (s *Store) commit(ctx context.Context, apply func(*State) bool) (State, error) {
	var st State
	err := cache.WithLock(ctx, s.dir, "state", func() error {
		var err error
		if st, err = s.load(); err != nil {
			return err
		}
		before := st.Leases.clone()
		if !apply(&st) {
			return nil
		}
		if err := s.save(st); err != nil {
			return err
		}
		return s.settle(before, st.Leases)
	})
	return st, err
}

func (s *Store) load() (State, error) {
	path := filepath.Join(s.dir, stateFile)
	var st State
	data, err := os.ReadFile(path) //nolint:gosec // under the machine's own cache dir
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return State{}, fmt.Errorf("prstate: read %s: %w", path, err)
	default:
		if err := json.Unmarshal(data, &st); err != nil {
			return State{}, fmt.Errorf("prstate: parse %s: %w", path, err)
		}
	}
	st.Leases, err = s.leases()
	return st, err
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

func (s *Store) renew(want Want, now time.Time) error {
	for _, n := range want.PRs {
		if err := s.touch(prLease(n), now); err != nil {
			return err
		}
	}
	for _, prefix := range want.Prefixes {
		if err := s.touch(laneLease(prefix), now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) leases() (Leases, error) {
	l := Leases{PRs: map[int]time.Time{}, Prefixes: map[string]time.Time{}}
	dir := filepath.Join(s.dir, leaseDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("prstate: list %s: %w", dir, err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return l, fmt.Errorf("prstate: stat lease %s: %w", e.Name(), err)
		}
		at := info.ModTime().UTC()
		if kind, key, ok := strings.Cut(e.Name(), "-"); ok && kind == "pr" {
			if n, err := strconv.Atoi(key); err == nil {
				l.PRs[n] = at
			}
		} else if ok && kind == "lane" {
			if prefix, err := base64.RawURLEncoding.DecodeString(key); err == nil {
				l.Prefixes[string(prefix)] = at
			}
		}
	}
	return l, nil
}

func (s *Store) settle(before, after Leases) error {
	for n, at := range after.PRs {
		if was, ok := before.PRs[n]; !ok || !was.Equal(at) {
			if err := s.touch(prLease(n), at); err != nil {
				return err
			}
		}
	}
	for n, at := range before.PRs {
		if _, kept := after.PRs[n]; !kept {
			if err := s.drop(prLease(n), at); err != nil {
				return err
			}
		}
	}
	for prefix, at := range before.Prefixes {
		if _, kept := after.Prefixes[prefix]; !kept {
			if err := s.drop(laneLease(prefix), at); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) touch(name string, at time.Time) error {
	path := filepath.Join(s.dir, leaseDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // under the machine's own cache dir
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Chtimes(path, at, at)
	}
	if err != nil {
		return fmt.Errorf("prstate: renew lease %s: %w", path, err)
	}
	return nil
}

func (s *Store) drop(name string, at time.Time) error {
	path := filepath.Join(s.dir, leaseDir, name)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.ModTime().Equal(at) {
		return nil
	}
	if err == nil {
		err = os.Remove(path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prstate: drop lease %s: %w", path, err)
	}
	return nil
}

func prLease(n int) string { return "pr-" + strconv.Itoa(n) }

func laneLease(prefix string) string {
	return "lane-" + base64.RawURLEncoding.EncodeToString([]byte(prefix))
}

func (l Leases) clone() Leases {
	return Leases{PRs: maps.Clone(l.PRs), Prefixes: maps.Clone(l.Prefixes)}
}

// Covers reports whether st holds a record for everything want names, however
// old, so a reader the budget refused can serve it labelled stale.
func (st State) Covers(want Want) bool {
	for _, n := range want.PRs {
		if pr, ok := st.PRs[n]; !ok || pr.PolledAt.IsZero() {
			return false
		}
	}
	for _, prefix := range want.Prefixes {
		lane, ok := st.Lanes[prefix]
		if !ok || !st.Covers(Want{PRs: lane.PRs}) {
			return false
		}
	}
	return true
}

func (st State) fresh(want Want, now time.Time) bool {
	for _, n := range want.PRs {
		pr, ok := st.PRs[n]
		if !ok || pr.PushedAt.After(pr.PolledAt) || !pr.settled() && now.Sub(pr.PolledAt) >= MinInterval {
			return false
		}
	}
	for _, prefix := range want.Prefixes {
		lane, ok := st.Lanes[prefix]
		if !ok || now.Sub(lane.PolledAt) >= MinInterval || !st.fresh(Want{PRs: lane.PRs}, now) {
			return false
		}
	}
	return true
}

func (st State) unpolled(want Want, now time.Time) ([]int, bool) {
	if st.AttemptedAt.IsZero() || !st.fresh(Want{Prefixes: want.Prefixes}, now) {
		return nil, false
	}
	var prs []int
	for _, n := range want.PRs {
		pr, ok := st.PRs[n]
		switch {
		case !ok || pr.PushedAt.After(pr.PolledAt):
			prs = append(prs, n)
		case !st.fresh(Want{PRs: []int{n}}, now):
			return nil, false
		}
	}
	return prs, len(prs) > 0
}

func (st State) unread(prefixes []string, now time.Time) []int {
	var unread []int
	for _, prefix := range prefixes {
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
	newest := !st.PolledAt.After(now)
	if newest {
		st.PolledAt, st.Trunk, st.Rate = now, p.trunk, p.rate
	}
	if st.PRs == nil {
		st.PRs = map[int]PR{}
	}
	if st.Lanes == nil {
		st.Lanes = map[string]Lane{}
	}
	for n, pr := range p.prs {
		was := st.PRs[n]
		if was.PolledAt.After(now) {
			continue
		}
		pr.PolledAt = now
		if was.PushedHead != "" && pr.HeadRefOid != was.PushedHead && now.Sub(was.PushedAt) < pushLag {
			pr.PushedHead, pr.PushedAt = was.PushedHead, was.PushedAt
		}
		st.PRs[n] = pr
	}
	for prefix, prs := range p.lanes {
		if st.Lanes[prefix].PolledAt.After(now) {
			continue
		}
		st.Lanes[prefix] = Lane{PRs: prs, PolledAt: now}
		for _, n := range prs {
			if st.Leases.PRs[n].Before(now) {
				st.Leases.PRs[n] = now
			}
		}
	}
	for _, n := range p.missing {
		if was := st.PRs[n]; was.PolledAt.After(now) || was.PushedAt.After(now) {
			continue
		}
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
	if newest && (st.Backoff == nil || !st.Backoff.Since.After(now)) {
		st.Backoff = quotaBackoff(p.rate, now)
	}
}

func quotaBackoff(rate Rate, now time.Time) *Backoff {
	if rate.Remaining >= rateFloor || !rate.ResetAt.After(now) {
		return nil
	}
	return &Backoff{Since: now, Until: rate.ResetAt, ProbeAt: rate.ResetAt, Reason: "quota below the floor"}
}

func (pr PR) settled() bool {
	return pr.State != "" && pr.State != "OPEN" && len(pr.SquashOn) > 0 && pr.Graphite != nil
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
