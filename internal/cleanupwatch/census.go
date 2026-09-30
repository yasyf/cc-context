package cleanupwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

const censusWidth = 8

// Verdicts a census assigns each watcher. None of them deletes anything: a
// root or daemon is retired only through [Retire] for an authorized exact
// worktree removal, after a fresh ownership gate.
const (
	// VerdictBusy marks a root with live subscriptions, triggers, or queries.
	VerdictBusy = "busy"
	// VerdictRetirable marks a watcher an authorized removal of its worktree
	// would retire once the fresh gate passes.
	VerdictRetirable = "retire-on-removal"
	// VerdictMissing marks a root whose directory is gone from disk.
	VerdictMissing = "path-missing"
	// VerdictOrphan marks a daemon whose socket, Git directory, or worktree is
	// gone from disk.
	VerdictOrphan = "orphan"
	// VerdictUnresolved marks a daemon whose worktree cannot be named from its
	// socket and Git metadata.
	VerdictUnresolved = "unresolved"
	// VerdictUnknown marks a root that vanished or failed mid-census.
	VerdictUnknown = "unknown"
)

// Report is one on-demand census of every Watchman root and builtin fsmonitor
// daemon.
type Report struct {
	Watchman  Watchman `json:"watchman"`
	FSMonitor []Daemon `json:"fsmonitor"`
}

// Watchman is the Watchman server's state; Available is false, with Absent
// saying why, when Watchman is not installed or not running.
type Watchman struct {
	Available bool   `json:"available"`
	Absent    string `json:"absent,omitempty"`
	Version   string `json:"version,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Roots     []Root `json:"roots"`
	// Unexamined lists the roots past [Deps.MaxRoots], by path only.
	Unexamined []string `json:"unexamined,omitempty"`
	Clients    []Client `json:"clients"`
	// ClientBound is the [Deps.MaxClients] the census ran under.
	ClientBound int `json:"client_bound"`
}

// Root is one Watchman root: its canonical path, the config it loaded when it
// was created, and every consumer attached to it.
type Root struct {
	Path          string          `json:"path"`
	Watcher       string          `json:"watcher"`
	FSType        string          `json:"fstype"`
	UptimeSeconds int64           `json:"uptime_s"`
	Missing       bool            `json:"missing,omitempty"`
	Config        json.RawMessage `json:"config,omitempty"`
	DiskConfig    json.RawMessage `json:"disk_config,omitempty"`
	// StaleConfig reports that the on-disk .watchmanconfig differs from the
	// loaded one; Watchman rereads it only when the root is recreated.
	StaleConfig     bool           `json:"stale_config"`
	DiskConfigError string         `json:"disk_config_error,omitempty"`
	Recrawls        int            `json:"recrawls"`
	RecrawlReason   string         `json:"recrawl_reason,omitempty"`
	RecrawlWarning  string         `json:"recrawl_warning,omitempty"`
	ShouldRecrawl   bool           `json:"should_recrawl,omitempty"`
	CrawlMS         int64          `json:"crawl_ms"`
	Crawling        bool           `json:"crawling,omitempty"`
	Subscriptions   []Subscription `json:"subscriptions"`
	Triggers        []Trigger      `json:"triggers"`
	Queries         []Query        `json:"queries"`
	Error           string         `json:"error,omitempty"`
	Verdict         string         `json:"verdict"`
}

// Subscription is one client subscription on a root.
type Subscription struct {
	Name   string `json:"name"`
	PID    int    `json:"pid,omitempty"`
	Client string `json:"client,omitempty"`
}

// Trigger is one registered trigger: a command Watchman runs on change.
type Trigger struct {
	Name    string   `json:"name"`
	Command []string `json:"command,omitempty"`
}

// Query is one query in flight on a root.
type Query struct {
	PID       int    `json:"pid"`
	State     string `json:"state"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// Client is one connection to the Watchman server; Roots lists the roots it
// subscribes to. Self marks the census's own debug-status request, matched by
// the exact pid it ran as; Unexamined marks a connection whose pid is past
// [Deps.MaxClients] and was not looked up.
type Client struct {
	PID        int      `json:"pid,omitempty"`
	Name       string   `json:"name,omitempty"`
	State      string   `json:"state"`
	Command    string   `json:"command,omitempty"`
	Protected  bool     `json:"protected,omitempty"`
	Gone       bool     `json:"gone,omitempty"`
	Self       bool     `json:"self,omitempty"`
	Unexamined bool     `json:"unexamined,omitempty"`
	Roots      []string `json:"roots,omitempty"`
}

// Take runs one bounded census: Watchman through --no-spawn commands only, and
// the fsmonitor daemons through their IPC sockets and Git metadata. It reads
// nothing but each root's .watchmanconfig and each daemon's Git directory.
func Take(ctx context.Context, d Deps) (Report, error) {
	w, err := d.censusWatchman(ctx)
	if err != nil {
		return Report{}, err
	}
	daemons, err := d.resolvedDaemons(ctx)
	if err != nil {
		return Report{}, err
	}
	for i := range daemons {
		if daemons[i].Verdict != VerdictRetirable {
			continue
		}
		config, err := d.fsmonitorConfig(ctx, daemons[i].Worktree)
		if err != nil {
			return Report{}, err
		}
		daemons[i].Config = config
	}
	return Report{Watchman: w, FSMonitor: daemons}, nil
}

func (d Deps) censusWatchman(ctx context.Context) (Watchman, error) {
	srv, err := d.probe(ctx)
	if err != nil {
		return Watchman{}, err
	}
	if srv.Absent != "" {
		return Watchman{Absent: srv.Absent, Version: srv.Version}, nil
	}
	status, err := d.debugStatus(ctx)
	if err != nil {
		return Watchman{}, err
	}
	w := Watchman{Available: true, Version: srv.Version, PID: srv.PID, ClientBound: d.MaxClients}
	examine := status.Roots
	if len(examine) > d.MaxRoots {
		for _, r := range examine[d.MaxRoots:] {
			w.Unexamined = append(w.Unexamined, r.Path)
		}
		examine = examine[:d.MaxRoots]
	}
	w.Roots = make([]Root, len(examine))
	var g errgroup.Group
	g.SetLimit(censusWidth)
	for i, raw := range examine {
		g.Go(func() error {
			w.Roots[i] = d.inspectRoot(ctx, raw)
			return ctx.Err()
		})
	}
	if err := g.Wait(); err != nil {
		return Watchman{}, err
	}
	subscribed := map[int][]string{}
	for _, root := range w.Roots {
		for _, s := range root.Subscriptions {
			if s.PID != 0 {
				subscribed[s.PID] = append(subscribed[s.PID], root.Path)
			}
		}
	}
	w.Clients, err = d.inspectClients(ctx, status)
	if err != nil {
		return Watchman{}, err
	}
	for i := range w.Clients {
		w.Clients[i].Roots = subscribed[w.Clients[i].PID]
	}
	return w, nil
}

func (d Deps) inspectRoot(ctx context.Context, raw rawRoot) Root {
	root := rootFromStatus(raw)
	c, err := d.consumers(ctx, raw.Path)
	if err != nil {
		root.Error, root.Verdict = err.Error(), VerdictUnknown
		return root
	}
	root.Subscriptions, root.Triggers = c.Subscriptions, c.Triggers
	root.Config, err = d.loadedConfig(ctx, raw.Path)
	if err != nil {
		root.Error, root.Verdict = err.Error(), VerdictUnknown
		return root
	}
	disk := snapshotConfig(raw.Path)
	root.DiskConfig, root.DiskConfigError = disk.Raw, disk.Error
	root.StaleConfig = disk.Error != "" || !sameConfig(root.Config, disk.Raw)
	switch {
	case len(root.Subscriptions) > 0 || len(root.Triggers) > 0 || len(root.Queries) > 0:
		root.Verdict = VerdictBusy
	case root.Missing:
		root.Verdict = VerdictMissing
	default:
		root.Verdict = VerdictRetirable
	}
	return root
}

func rootFromStatus(raw rawRoot) Root {
	root := Root{
		Path:          raw.Path,
		Watcher:       raw.Watcher,
		FSType:        raw.FSType,
		UptimeSeconds: raw.Uptime,
		Recrawls:      raw.Recrawl.Count,
		RecrawlReason: raw.Recrawl.Reason,
		ShouldRecrawl: raw.Recrawl.ShouldRecrawl,
		Queries:       queriesOf(raw.Queries),
	}
	if raw.Recrawl.Warning != nil {
		root.RecrawlWarning = *raw.Recrawl.Warning
	}
	root.CrawlMS, root.Crawling = crawl(raw)
	if _, err := os.Lstat(raw.Path); errors.Is(err, fs.ErrNotExist) {
		root.Missing = true
	}
	return root
}

func crawl(raw rawRoot) (int64, bool) {
	started, completed := raw.Recrawl.Started, raw.Recrawl.Completed
	if started == nil {
		return 0, !raw.DoneInitial
	}
	if completed == nil || *completed < *started {
		return 0, true
	}
	return *completed - *started, false
}

func queriesOf(raw []rawQuery) []Query {
	queries := make([]Query, 0, len(raw))
	for _, q := range raw {
		queries = append(queries, Query{PID: q.ClientPID, State: q.State, ElapsedMS: q.Elapsed})
	}
	return queries
}

func (d Deps) inspectClients(ctx context.Context, status watchmanStatus) ([]Client, error) {
	clients := make([]Client, len(status.Clients))
	var lookup []int
	for i, raw := range status.Clients {
		c := Client{State: raw.State}
		if raw.Peer != nil {
			c.PID, c.Name = raw.Peer.PID, raw.Peer.Name
		}
		switch {
		case c.PID == 0:
		case c.PID == status.SelfPID:
			c.Self = true
		case slices.Contains(lookup, c.PID):
		case len(lookup) < d.MaxClients:
			lookup = append(lookup, c.PID)
		default:
			c.Unexamined = true
		}
		clients[i] = c
	}
	procs, err := d.Procs.Processes(ctx, lookup)
	if err != nil {
		return nil, err
	}
	for i, c := range clients {
		if c.PID == 0 || c.Self || c.Unexamined {
			continue
		}
		proc, ok := procs[c.PID]
		if !ok {
			clients[i].Gone = true
			continue
		}
		clients[i].Command = proc.Command
		clients[i].Protected = d.protected(c.Name) || d.protected(proc.Command)
	}
	return clients, nil
}

// CheckQuarantine reports [ErrWatched] when dir sits at or below a Watchman
// root or an fsmonitor daemon's worktree, or contains one — a quarantine there
// would feed every deletion back through a watcher — and when any live
// fsmonitor daemon's worktree cannot be resolved to rule it out. dir must exist.
func CheckQuarantine(ctx context.Context, d Deps, dir string) error {
	id, err := resolveTarget(dir)
	if err != nil {
		return err
	}
	var reasons []string
	srv, err := d.probe(ctx)
	if err != nil {
		return err
	}
	if srv.Absent == "" {
		roots, err := d.watchList(ctx)
		if err != nil {
			return err
		}
		for _, r := range roots {
			if under(id.Path, r) || under(r, id.Path) {
				reasons = append(reasons, "watchman root "+r)
			}
		}
	}
	daemons, err := d.resolvedDaemons(ctx)
	if err != nil {
		return err
	}
	for _, dm := range daemons {
		switch {
		case dm.Verdict == VerdictUnresolved:
			reasons = append(reasons, fmt.Sprintf("fsmonitor daemon %d has an unresolved worktree: %s", dm.PID, dm.Error))
		case dm.Worktree == "":
			reasons = append(reasons, fmt.Sprintf("fsmonitor daemon %d lost its socket %s and names no worktree", dm.PID, dm.Socket))
		case under(id.Path, dm.Worktree) || under(dm.Worktree, id.Path):
			reasons = append(reasons, fmt.Sprintf("fsmonitor daemon %d watches %s", dm.PID, dm.Worktree))
		}
	}
	if len(reasons) > 0 {
		return fmt.Errorf("%w: %s: %s", ErrWatched, id.Path, strings.Join(reasons, "; "))
	}
	return nil
}

// Render formats r as one line per watcher.
func (r Report) Render() string {
	var b strings.Builder
	w := r.Watchman
	if !w.Available {
		fmt.Fprintf(&b, "watchman: unavailable (%s)\n", w.Absent)
	} else {
		fmt.Fprintf(&b, "watchman %s (pid %d): %d roots, %d clients\n", w.Version, w.PID, len(w.Roots)+len(w.Unexamined), len(w.Clients))
		for _, root := range w.Roots {
			fmt.Fprintf(&b, "  %s  %s  %s/%s  recrawls %d", root.Verdict, root.Path, root.Watcher, root.FSType, root.Recrawls)
			if root.RecrawlReason != "" {
				fmt.Fprintf(&b, " (%s)", root.RecrawlReason)
			}
			if root.Crawling {
				b.WriteString("  crawling")
			} else {
				fmt.Fprintf(&b, "  crawl %s", time.Duration(root.CrawlMS)*time.Millisecond)
			}
			fmt.Fprintf(&b, "  subs %d triggers %d queries %d", len(root.Subscriptions), len(root.Triggers), len(root.Queries))
			if root.StaleConfig {
				b.WriteString("  config stale")
			}
			if root.DiskConfigError != "" {
				fmt.Fprintf(&b, " (%s)", root.DiskConfigError)
			}
			if root.Error != "" {
				fmt.Fprintf(&b, "  error: %s", root.Error)
			}
			b.WriteByte('\n')
		}
		if len(w.Unexamined) > 0 {
			fmt.Fprintf(&b, "  %d roots not examined (census bounded at %d):\n", len(w.Unexamined), len(w.Roots))
			for _, p := range w.Unexamined {
				fmt.Fprintf(&b, "    %s\n", p)
			}
		}
		for _, c := range w.Clients {
			fmt.Fprintf(&b, "  client pid %d %s [%s]", c.PID, c.Name, c.State)
			switch {
			case c.Self:
				b.WriteString(" self")
			case c.Unexamined:
				fmt.Fprintf(&b, " not examined (bounded at %d pids)", w.ClientBound)
			case c.Gone:
				b.WriteString(" gone")
			}
			if c.Protected {
				b.WriteString(" protected")
			}
			if len(c.Roots) > 0 {
				fmt.Fprintf(&b, " subscribes %s", strings.Join(c.Roots, ", "))
			}
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "fsmonitor: %d daemons\n", len(r.FSMonitor))
	for _, dm := range r.FSMonitor {
		fmt.Fprintf(&b, "  %s  pid %d", dm.Verdict, dm.PID)
		if dm.Worktree != "" {
			fmt.Fprintf(&b, "  %s", dm.Worktree)
		}
		if dm.Socket != "" {
			fmt.Fprintf(&b, "  socket %s", dm.Socket)
		}
		if dm.Error != "" {
			fmt.Fprintf(&b, "  (%s)", dm.Error)
		}
		if n := len(dm.Config); n > 0 {
			eff := dm.Config[n-1]
			fmt.Fprintf(&b, "  core.fsmonitor=%s (%s %s)", eff.Value, eff.Scope, eff.Origin)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
