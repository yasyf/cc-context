package ghapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// Floor is how much of a pool ccx leaves unread: a read that finds its pool
// under Floor, or a fifth of a smaller pool's limit, before the pool resets is
// refused, so the rest stays for writes.
const Floor = 1000

const quotaStale = time.Minute

// Resources name GitHub's rate-limit pools.
const (
	ResourceCore    = "core"
	ResourceGraphQL = "graphql"
	ResourceSearch  = "search"
)

// Pool is one identity's rate limit for one GitHub resource, and the callers
// charged against it since it last reset, most requests first.
type Pool struct {
	Identity  string    `json:"identity"`
	Resource  string    `json:"resource"`
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Floor     int       `json:"floor"`
	ResetAt   time.Time `json:"resetAt"`
	ReadAt    time.Time `json:"readAt"`
	Callers   []Caller  `json:"callers"`
}

// Caller is one caller's requests against a pool since the pool last reset.
type Caller struct {
	Caller   string `json:"caller"`
	Requests int    `json:"requests"`
}

// FloorError is a read refused because its pool is under Floor until ResetAt.
type FloorError struct {
	Pool Pool
}

func (e *FloorError) Error() string {
	p := e.Pool
	reset := p.ResetAt.Local()
	msg := fmt.Sprintf("%s %s has %d of %d left, under the floor of %d, until %s (%s)",
		p.Identity, p.Resource, p.Remaining, p.Limit, p.Floor, reset.Format(time.Kitchen), reset.Format(time.RFC3339))
	if top := TopCallers(p, 5); top != "" {
		msg += "; top callers: " + top
	}
	return msg
}

// TopCallers renders p's n heaviest callers as "name requests" pairs.
func TopCallers(p Pool, n int) string {
	parts := make([]string, 0, n)
	for _, c := range p.Callers[:min(n, len(p.Callers))] {
		parts = append(parts, fmt.Sprintf("%s %d", c.Caller, c.Requests))
	}
	return strings.Join(parts, ", ")
}

type poolState struct {
	Limit     int                       `json:"limit"`
	Remaining int                       `json:"remaining"`
	ResetAt   time.Time                 `json:"resetAt"`
	ReadAt    time.Time                 `json:"readAt"`
	Callers   map[string]int            `json:"callers,omitempty"`
	Paths     map[string]map[string]int `json:"paths,omitempty"`

	closed []closedWindow
}

// closedWindow is one finished rate-limit window of a pool and who spent it, as
// HistoryFile keeps it.
type closedWindow struct {
	Pool      string                    `json:"pool"`
	Limit     int                       `json:"limit"`
	Remaining int                       `json:"remaining"`
	ResetAt   time.Time                 `json:"resetAt"`
	ReadAt    time.Time                 `json:"readAt"`
	Callers   map[string]int            `json:"callers"`
	Paths     map[string]map[string]int `json:"paths,omitempty"`
}

func (p *poolState) roll(now time.Time, reset time.Time) {
	if !p.ResetAt.IsZero() && (!now.Before(p.ResetAt) || reset.After(p.ResetAt.Add(time.Minute))) {
		if len(p.Callers) > 0 {
			p.closed = append(p.closed, closedWindow{Limit: p.Limit, Remaining: p.Remaining, ResetAt: p.ResetAt, ReadAt: p.ReadAt, Callers: p.Callers, Paths: p.Paths})
		}
		p.Callers, p.Paths = nil, nil
	}
}

func (p *poolState) floor() int { return min(Floor, p.Limit/5) }

func (p *poolState) observe(now time.Time, limit, remaining int, reset time.Time) {
	reset = reset.UTC()
	switch {
	case reset.Before(p.ResetAt.Add(-time.Second)):
	case !p.ReadAt.IsZero() && reset.Sub(p.ResetAt).Abs() <= time.Second:
		p.Limit, p.Remaining, p.ReadAt = limit, min(p.Remaining, remaining), now.UTC()
	default:
		p.roll(now, reset)
		p.Limit, p.Remaining, p.ResetAt, p.ReadAt = limit, remaining, reset, now.UTC()
	}
}

func (p *poolState) stale(now time.Time) bool {
	return now.Sub(p.ReadAt) > quotaStale || !now.Before(p.ResetAt)
}

type ledger struct {
	dir string
	now func() time.Time

	mu   sync.Mutex
	open *ledgerBatch
}

// ledgerBatch is the updates of one process that one hold of the ledger lock
// applies together: it takes updates until its leader holds the lock.
type ledgerBatch struct {
	applies []func(map[string]*poolState)
	done    chan struct{}
	err     error
}

// QuotaDir is the machine-wide directory holding the quota ledger and the
// response cache. It ignores $CLAUDE_PLUGIN_DATA so plugin and shell
// processes share one budget.
func QuotaDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("ghapi: resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "cc-context", "github-quota"), nil
}

func defaultLedger() *ledger {
	dir, err := QuotaDir()
	if err != nil {
		return nil
	}
	return &ledger{dir: dir, now: time.Now}
}

func poolKey(identity, resource string) string { return identity + ":" + resource }

func (l *ledger) load() (map[string]*poolState, error) {
	pools := map[string]*poolState{}
	data, err := os.ReadFile(filepath.Join(l.dir, "ledger.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return pools, nil
	case err != nil:
		return nil, fmt.Errorf("ghapi: read quota ledger: %w", err)
	}
	if err := json.Unmarshal(data, &pools); err != nil {
		return nil, fmt.Errorf("ghapi: parse quota ledger: %w", err)
	}
	return pools, nil
}

// update applies apply to the ledger. Concurrent updates in one process join
// one batch, so the process takes the machine-wide lock once per batch rather
// than once per request.
func (l *ledger) update(ctx context.Context, apply func(map[string]*poolState)) error {
	l.mu.Lock()
	b := l.open
	lead := b == nil
	if lead {
		b = &ledgerBatch{done: make(chan struct{})}
		l.open = b
	}
	b.applies = append(b.applies, apply)
	l.mu.Unlock()
	if !lead {
		select {
		case <-b.done:
			return b.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.err = l.flush(ctx, b)
	l.mu.Lock()
	if l.open == b {
		l.open = nil
	}
	l.mu.Unlock()
	close(b.done)
	return b.err
}

func (l *ledger) flush(ctx context.Context, b *ledgerBatch) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("ghapi: create %s: %w", l.dir, err)
	}
	return cache.WithLock(ctx, l.dir, "ledger", func() error {
		l.mu.Lock()
		l.open = nil
		applies := b.applies
		l.mu.Unlock()
		pools, err := l.load()
		if err != nil {
			return err
		}
		for _, apply := range applies {
			apply(pools)
		}
		if err := l.keepClosedWindows(pools); err != nil {
			return err
		}
		data, err := json.Marshal(pools)
		if err != nil {
			return fmt.Errorf("ghapi: encode quota ledger: %w", err)
		}
		return cache.Store(filepath.Join(l.dir, "ledger.json"), data, 0o600)
	})
}

// HistoryFile is the JSON Lines file under QuotaDir holding every finished
// rate-limit window and its callers, since the ledger clears a pool's callers
// when it resets. Past historyMaxBytes it moves to HistoryFile + ".1", replacing
// the older one, which bounds the two at twice that.
const HistoryFile = "ledger-history.jsonl"

const historyMaxBytes = 16 << 20

func (l *ledger) keepClosedWindows(pools map[string]*poolState) error {
	var lines []byte
	for key, p := range pools {
		for _, window := range p.closed {
			window.Pool = key
			line, err := json.Marshal(window)
			if err != nil {
				return fmt.Errorf("ghapi: encode a finished window: %w", err)
			}
			lines = append(append(lines, line...), '\n')
		}
		p.closed = nil
	}
	if len(lines) == 0 {
		return nil
	}
	path := filepath.Join(l.dir, HistoryFile)
	if info, err := os.Stat(path); err == nil && info.Size() > historyMaxBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return fmt.Errorf("ghapi: rotate quota history: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // under the machine's own cache dir
	if err != nil {
		return fmt.Errorf("ghapi: open quota history: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(lines); err != nil {
		return fmt.Errorf("ghapi: append quota history: %w", err)
	}
	return nil
}

func (l *ledger) pool(pools map[string]*poolState, identity, resource string) *poolState {
	key := poolKey(identity, resource)
	if pools[key] == nil {
		pools[key] = &poolState{}
	}
	return pools[key]
}

func (l *ledger) charge(ctx context.Context, identity, resource, caller, target string, header http.Header) error {
	if r := header.Get("X-RateLimit-Resource"); r != "" {
		resource = r
	}
	return l.update(ctx, func(pools map[string]*poolState) {
		now := l.now()
		p := l.pool(pools, identity, resource)
		limit, limitErr := strconv.Atoi(header.Get("X-RateLimit-Limit"))
		remaining, remainingErr := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
		reset, resetErr := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
		if limitErr == nil && remainingErr == nil && resetErr == nil {
			p.observe(now, limit, remaining, time.Unix(reset, 0))
		} else {
			p.roll(now, p.ResetAt)
			p.Remaining = max(p.Remaining-1, 0)
		}
		if p.Callers == nil {
			p.Callers = map[string]int{}
		}
		p.Callers[caller]++
		if target == "" {
			return
		}
		if p.Paths == nil {
			p.Paths = map[string]map[string]int{}
		}
		if p.Paths[caller] == nil {
			p.Paths[caller] = map[string]int{}
		}
		p.Paths[caller][FoldPath(target)]++
	})
}

var pathFolds = []struct {
	pattern *regexp.Regexp
	to      string
}{
	{regexp.MustCompile(`[?#].*$`), ""},
	{regexp.MustCompile(`^[a-z]+://[^/]+`), ""},
	{regexp.MustCompile(`/compare/.*$`), "/compare/:refs"},
	{regexp.MustCompile(`/(branches|heads|refs?)/.*$`), "/$1/:ref"},
	{regexp.MustCompile(`/[0-9a-f]{40}(/|$)`), "/:sha$1"},
	{regexp.MustCompile(`/[0-9]+(/|$)`), "/:n$1"},
}

// FoldPath is target with its query, host, numbers, commits and refs folded
// away, so one read counts as one path across every pull request it visits.
func FoldPath(target string) string {
	for _, fold := range pathFolds {
		target = fold.pattern.ReplaceAllString(target, fold.to)
	}
	return strings.TrimPrefix(target, "/")
}

// Spend is one caller's requests against one open pool, and on one path when
// the read asked for paths.
type Spend struct {
	Pool     string
	Caller   string
	Path     string
	Requests int
}

// LedgerSpend reads who spent each open pool from this machine's ledger,
// heaviest first, without a request to GitHub.
func LedgerSpend(byPath bool) ([]Spend, error) {
	l := defaultLedger()
	if l == nil {
		return nil, errors.New("ghapi: no user cache dir holds a quota ledger")
	}
	states, err := l.load()
	if err != nil {
		return nil, err
	}
	now := l.now()
	var spends []Spend
	for key, p := range states {
		if !now.Before(p.ResetAt) {
			continue
		}
		for caller, requests := range p.Callers {
			if !byPath || len(p.Paths[caller]) == 0 {
				spends = append(spends, Spend{Pool: key, Caller: caller, Requests: requests})
				continue
			}
			for path, n := range p.Paths[caller] {
				spends = append(spends, Spend{Pool: key, Caller: caller, Path: path, Requests: n})
			}
		}
	}
	slices.SortFunc(spends, func(a, b Spend) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.Caller, b.Caller), cmp.Compare(a.Path, b.Path))
	})
	return spends, nil
}

type rateLimits struct {
	Resources map[string]struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Reset     int64 `json:"reset"`
	} `json:"resources"`
}

func (l *ledger) record(ctx context.Context, identity string, limits rateLimits) error {
	return l.update(ctx, func(pools map[string]*poolState) {
		now := l.now()
		for resource, r := range limits.Resources {
			if resource != ResourceCore && resource != ResourceGraphQL && resource != ResourceSearch {
				continue
			}
			l.pool(pools, identity, resource).observe(now, r.Limit, r.Remaining, time.Unix(r.Reset, 0))
		}
	})
}

func (l *ledger) pools(principal string) ([]Pool, error) {
	states, err := l.load()
	if err != nil {
		return nil, err
	}
	now := l.now()
	var out []Pool
	for key, p := range states {
		id, resource, _ := strings.Cut(key, ":")
		if id != principal {
			continue
		}
		out = append(out, p.public(principal, resource, now))
	}
	slices.SortFunc(out, func(a, b Pool) int { return cmp.Compare(a.Resource, b.Resource) })
	return out, nil
}

func (p *poolState) public(principal, resource string, now time.Time) Pool {
	kind, _, _ := strings.Cut(principal, "#")
	pool := Pool{Identity: kind, Resource: resource, Limit: p.Limit, Remaining: p.Remaining, Floor: p.floor(), ResetAt: p.ResetAt, ReadAt: p.ReadAt, Callers: []Caller{}}
	if !now.Before(p.ResetAt) {
		pool.Remaining = p.Limit
		return pool
	}
	for name, n := range p.Callers {
		pool.Callers = append(pool.Callers, Caller{Caller: name, Requests: n})
	}
	slices.SortFunc(pool.Callers, func(a, b Caller) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Caller, b.Caller))
	})
	return pool
}

// Resource names the pool a request to target draws from.
func Resource(target string) string {
	path := target
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
		if j := strings.IndexByte(path, '/'); j >= 0 {
			path = path[j:]
		}
	}
	path = strings.TrimPrefix(path, "/")
	switch {
	case path == "graphql":
		return ResourceGraphQL
	case strings.HasPrefix(path, "search/"):
		return ResourceSearch
	default:
		return ResourceCore
	}
}

// Admit refuses a read from identity's resource pool with a *FloorError when
// the pool is under its floor, re-reading a stale pool from rate_limit first.
// The app identity is the installation on c's repository.
func (c *Client) Admit(ctx context.Context, identity, resource string) error {
	return c.admit(ctx, identity, resource, (*poolState).floor)
}

// AdmitSpill refuses an app read moved onto the user's resource pool once that
// pool is under half its limit, so one spent installation cannot also spend
// the user's own quota down to Floor.
func (c *Client) AdmitSpill(ctx context.Context, resource string) error {
	return c.admit(ctx, IdentityUser, resource, func(p *poolState) int { return p.Limit / 2 })
}

func (c *Client) admit(ctx context.Context, identity, resource string, floor func(*poolState) int) error {
	if c.quota == nil || c.floorless {
		return nil
	}
	principal, err := c.principal(ctx, identity)
	if err != nil {
		return err
	}
	states, err := c.quota.load()
	if err != nil {
		return err
	}
	now := c.quota.now()
	p, ok := states[poolKey(principal, resource)]
	if !ok || p.stale(now) {
		if err := c.readQuota(ctx, identity, principal); err != nil {
			return err
		}
		if states, err = c.quota.load(); err != nil {
			return err
		}
		if p, ok = states[poolKey(principal, resource)]; !ok {
			return nil
		}
	}
	if p.Remaining >= floor(p) || !now.Before(p.ResetAt) {
		return nil
	}
	pool := p.public(principal, resource, now)
	pool.Floor = floor(p)
	return &FloorError{Pool: pool}
}

// Charge counts one request by the calling process against identity's
// resource pool, for a request ccx did not send itself, such as gh's. target
// names what it asked for, a path or a gh command.
func (c *Client) Charge(ctx context.Context, identity, resource, target string) error {
	if c.quota == nil {
		return nil
	}
	principal, err := c.principal(ctx, identity)
	if err != nil {
		return err
	}
	return c.quota.charge(ctx, principal, resource, CallerName(ctx), target, http.Header{})
}

// Record charges one request another client in this process sent to target
// with identity's token, and reads the pool's balance from the response's
// rate-limit headers.
func (c *Client) Record(ctx context.Context, identity, target string, header http.Header) error {
	if c.quota == nil {
		return nil
	}
	principal, err := c.principal(ctx, identity)
	if err != nil {
		return err
	}
	return c.quota.charge(ctx, principal, Resource(target), CallerName(ctx), target, header)
}

// Pools reads identity's pools fresh from rate_limit and returns them with
// the callers the ledger charged since each last reset.
func (c *Client) Pools(ctx context.Context, identity string) ([]Pool, error) {
	if c.quota == nil {
		return nil, nil
	}
	principal, err := c.principal(ctx, identity)
	if err != nil {
		return nil, err
	}
	if err := c.readQuota(ctx, identity, principal); err != nil {
		return nil, err
	}
	return c.quota.pools(principal)
}

func (c *Client) principal(ctx context.Context, identity string) (string, error) {
	if identity != IdentityApp {
		return IdentityUser, nil
	}
	src, err := c.readApp(ctx)
	if err != nil || src == nil {
		return IdentityApp, err
	}
	_, id, err := src.token(ctx, c.repo, AppRefreshMargin, "")
	if err != nil {
		return "", err
	}
	return IdentityApp + "#" + strconv.FormatInt(id, 10), nil
}

func (c *Client) readQuota(ctx context.Context, identity, principal string) error {
	var cred credential = c.tokens
	if identity == IdentityApp {
		src, err := c.readApp(ctx)
		if err != nil {
			return err
		}
		if src == nil {
			return nil
		}
		cred = &appCredential{src: src, repo: c.repo}
	}
	token, err := cred.get(ctx)
	if err != nil {
		return err
	}
	status, _, payload, err := c.send(ctx, http.MethodGet, c.resolveRef("/rate_limit"), nil, token)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(http.MethodGet, "/rate_limit", status, http.Header{}, payload, time.Now())
	}
	var limits rateLimits
	if err := json.Unmarshal(payload, &limits); err != nil {
		return fmt.Errorf("ghapi: decode /rate_limit: %w", err)
	}
	return c.quota.record(ctx, principal, limits)
}

// CallerName names whoever a request is charged to: $CCX_CALLER, else
// $CCI_AGENT_ID, else the script or command that started this process.
func CallerName(ctx context.Context) string {
	for _, name := range []string{"CCX_CALLER", "CCI_AGENT_ID"} {
		if v := strings.TrimSpace(render.Getenv(ctx, name)); v != "" {
			return v
		}
	}
	return parentCaller()
}

var parentCaller = sync.OnceValue(func() string { return scriptName(parentArgv()) })

var interpreters = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "node": true, "bun": true, "deno": true, "ruby": true, "perl": true, "uv": true, "uvx": true}

func scriptName(argv []string) string {
	if len(argv) == 0 {
		return "unknown"
	}
	base := filepath.Base(argv[0])
	if !interpreters[base] && !strings.HasPrefix(base, "python") {
		return base
	}
	for _, arg := range argv[1:] {
		switch {
		case arg == "-c":
			return base
		case strings.HasPrefix(arg, "-") || arg == "run":
		default:
			return filepath.Base(arg)
		}
	}
	return base
}
