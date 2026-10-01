package cleanupwatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Retirement is the read-only plan for one exact worktree: the Watchman roots
// at or below it, the builtin fsmonitor daemon that owns it, and every reason
// the fresh gate refused. Dev and Ino pin the directory the plan was taken for.
type Retirement struct {
	Worktree string `json:"worktree"`
	Dev      uint64 `json:"dev"`
	Ino      uint64 `json:"ino"`
	// GitDir is the Git directory Git resolved for Worktree; the fsmonitor
	// stop is bound to it rather than rediscovered through Worktree.
	GitDir string `json:"git_dir,omitempty"`
	// Roots lists the roots to retire, deepest first.
	Roots []WatchedRoot `json:"roots"`
	// Ancestors lists the roots above the worktree, which are never retired.
	Ancestors []Coverage `json:"ancestors,omitempty"`
	FSMonitor *Daemon    `json:"fsmonitor,omitempty"`
	// Sockets lists every IPC socket an fsmonitor daemon for the worktree
	// could listen on.
	Sockets  []string `json:"sockets,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
}

// WatchedRoot is one Watchman root a retirement deletes, pinned by the
// device and inode of its directory when the plan was taken.
type WatchedRoot struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

// Refused reports whether the gate found any blocker.
func (r Retirement) Refused() bool {
	return len(r.Blockers) > 0
}

// Coverage is a root above the worktree; Ignored reports that the root's
// loaded ignore_dirs already exclude the worktree.
type Coverage struct {
	Root    string `json:"root"`
	Ignored bool   `json:"ignored"`
}

// Outcome records what a retirement or recreation changed, verified. A
// recreation that deleted its root but did not re-watch it lists the root in
// Retired with Recreated empty.
type Outcome struct {
	Retired   []string `json:"retired,omitempty"`
	Stopped   *Daemon  `json:"stopped,omitempty"`
	Recreated string   `json:"recreated,omitempty"`
}

type gateResult struct {
	Roots     []string
	Ancestors []Coverage
	Blockers  []string
}

// Plan takes the fresh, read-only retirement plan for worktree. It mutates
// nothing; a zero-consumer snapshot alone never authorizes a retirement.
func Plan(ctx context.Context, d Deps, worktree string) (Retirement, error) {
	id, err := resolveTarget(worktree)
	if err != nil {
		return Retirement{}, err
	}
	g, err := d.gate(ctx, id.Path, false)
	if err != nil {
		return Retirement{}, err
	}
	own, err := d.fsmonitorOwnership(ctx, id.Path)
	if err != nil {
		return Retirement{}, err
	}
	roots, unpinned := pinRoots(g.Roots)
	blockers := append(g.Blockers, unpinned...)
	return Retirement{
		Worktree:  id.Path,
		Dev:       id.Dev,
		Ino:       id.Ino,
		GitDir:    own.GitDir,
		Roots:     roots,
		Ancestors: g.Ancestors,
		FSMonitor: own.Owner,
		Sockets:   own.Sockets,
		Blockers:  append(blockers, own.Blockers...),
	}, nil
}

func pinRoots(paths []string) ([]WatchedRoot, []string) {
	roots := make([]WatchedRoot, 0, len(paths))
	var blockers []string
	for _, path := range paths {
		id, err := resolveTarget(path)
		switch {
		case err != nil:
			blockers = append(blockers, fmt.Sprintf("root %s cannot be pinned: %v", path, err))
		case id.Path != path:
			blockers = append(blockers, fmt.Sprintf("root %s resolves to %s", path, id.Path))
		default:
			roots = append(roots, WatchedRoot{Path: path, Dev: id.Dev, Ino: id.Ino})
		}
	}
	return roots, blockers
}

func rootPaths(roots []WatchedRoot) []string {
	paths := make([]string, len(roots))
	for i, r := range roots {
		paths[i] = r.Path
	}
	return paths
}

// Retire retires exactly what p names, for a worktree removal the caller has
// already authorized. It retakes the plan and refuses on any drift — a
// replaced worktree or root directory, a new root, a changed fsmonitor owner
// process, a late consumer. Then, per root, it checks the root's consumers,
// every non-ignoring ancestor's consumers, and every connected client; calls
// [Deps.Guard]; re-pins the worktree and root by device and inode; checks
// those consumers and clients once more; and deletes the root with a native
// watch-del it verifies. Last, it rechecks the fsmonitor owner and those
// consumers — refusing a daemon that appeared mid-retirement — calls
// [Deps.Guard], re-reads the owner's pid, start time, command, and socket —
// refusing any daemon that re-read cannot rule out as a second owner — and
// stops it through the Git directory it planned against, verifying it gone.
// Nothing else is touched. Watchman offers no atomic check-and-delete: the
// last consumer check and the watch-del are separate requests, so a
// subscription attaching between them is cancelled by the watch-del and
// leaves no evidence. The checks that follow prove only that each deleted root
// left the watch list and that no root at or below the worktree is watched
// again. Likewise a daemon that replaces the owner between the last re-read
// and the stop receives the stop. The checks that follow report a replacement
// that survives it, but one that exits on the stop passes them, so they cannot
// prove which process the stop reached.
func Retire(ctx context.Context, d Deps, p Retirement) (Outcome, error) {
	if d.Guard == nil {
		return Outcome{}, ErrNoGuard
	}
	if p.Refused() {
		return Outcome{}, refused(p.Blockers)
	}
	fresh, err := Plan(ctx, d, p.Worktree)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: replan %s: %w", ErrRefused, p.Worktree, err)
	}
	if drift := planDrift(p, fresh); len(drift) > 0 {
		return Outcome{}, refused(drift)
	}
	pin := identity{Path: p.Worktree, Dev: p.Dev, Ino: p.Ino}
	targets := rootPaths(fresh.Roots)
	var out Outcome
	for _, root := range fresh.Roots {
		retired, err := d.retireRoot(ctx, pin, root, targets)
		if err != nil {
			return out, err
		}
		if retired {
			out.Retired = append(out.Retired, root.Path)
		}
	}
	stopped, err := d.retireFSMonitor(ctx, pin, fresh)
	if err != nil {
		return out, err
	}
	out.Stopped = stopped
	if err := d.verifyNoRootsWithin(ctx, p.Worktree); err != nil {
		return out, err
	}
	return out, nil
}

func planDrift(p, fresh Retirement) []string {
	var drift []string
	if fresh.Worktree != p.Worktree || fresh.Dev != p.Dev || fresh.Ino != p.Ino {
		drift = append(drift, fmt.Sprintf("%s was replaced since planning (now %s dev %d ino %d)", p.Worktree, fresh.Worktree, fresh.Dev, fresh.Ino))
	}
	drift = append(drift, fresh.Blockers...)
	for _, root := range fresh.Roots {
		i := slices.IndexFunc(p.Roots, func(r WatchedRoot) bool { return r.Path == root.Path })
		switch {
		case i < 0:
			drift = append(drift, fmt.Sprintf("root %s appeared under %s since planning", root.Path, p.Worktree))
		case p.Roots[i] != root:
			drift = append(drift, fmt.Sprintf("root %s was replaced since planning (dev %d ino %d, now dev %d ino %d)", root.Path, p.Roots[i].Dev, p.Roots[i].Ino, root.Dev, root.Ino))
		}
	}
	if fresh.GitDir != p.GitDir {
		drift = append(drift, fmt.Sprintf("git dir of %s changed from %s to %s", p.Worktree, p.GitDir, fresh.GitDir))
	}
	if o := ownerDrift(p.Worktree, p.FSMonitor, fresh.FSMonitor); o != "" {
		drift = append(drift, o)
	}
	return drift
}

func ownerDrift(worktree string, planned, now *Daemon) string {
	switch {
	case (planned == nil) != (now == nil):
		return fmt.Sprintf("fsmonitor owner of %s changed since planning", worktree)
	case planned != nil && (!sameProcess(*planned, *now) || planned.Socket != now.Socket):
		return fmt.Sprintf("fsmonitor owner of %s changed from %s to %s", worktree, describe(*planned), describe(*now))
	}
	return ""
}

func (d Deps) retireRoot(ctx context.Context, pin identity, root WatchedRoot, targets []string) (bool, error) {
	blockers, watched, err := d.freshBlockers(ctx, pin.Path, root.Path, targets)
	if err != nil {
		return false, err
	}
	if len(blockers) > 0 {
		return false, refused(blockers)
	}
	if !watched {
		return false, nil
	}
	if err := d.guard(ctx, pin.Path); err != nil {
		return false, err
	}
	if err := repinRoot(pin, root); err != nil {
		return false, err
	}
	return d.deleteRoot(ctx, pin.Path, root.Path, targets)
}

func (d Deps) deleteRoot(ctx context.Context, worktree, root string, targets []string) (bool, error) {
	blockers, watched, err := d.freshBlockers(ctx, worktree, root, targets)
	if err != nil {
		return false, err
	}
	if len(blockers) > 0 {
		return false, refused(blockers)
	}
	if !watched {
		return false, nil
	}
	if err := d.watchDel(ctx, root); err != nil {
		return false, err
	}
	roots, err := d.watchList(ctx)
	if err != nil {
		return false, err
	}
	if slices.Contains(roots, root) {
		return false, fmt.Errorf("watchman still lists %s after watch-del", root)
	}
	return true, nil
}

func repinWorktree(pin identity) error {
	now, err := resolveTarget(pin.Path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRefused, pin.Path, err)
	}
	if now != pin {
		return fmt.Errorf("%w: %s was replaced mid-retirement", ErrRefused, pin.Path)
	}
	return nil
}

func repinRoot(pin identity, root WatchedRoot) error {
	if err := repinWorktree(pin); err != nil {
		return err
	}
	now, err := resolveTarget(root.Path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRefused, root.Path, err)
	}
	if now != identity(root) || !under(root.Path, pin.Path) {
		return fmt.Errorf("%w: root %s was replaced mid-retirement (now %s dev %d ino %d)", ErrRefused, root.Path, now.Path, now.Dev, now.Ino)
	}
	return nil
}

func (d Deps) retireFSMonitor(ctx context.Context, pin identity, fresh Retirement) (*Daemon, error) {
	own, err := d.fsmonitorOwnership(ctx, pin.Path)
	if err != nil {
		return nil, err
	}
	blockers := own.Blockers
	if own.GitDir != fresh.GitDir {
		blockers = append(blockers, fmt.Sprintf("git dir of %s changed to %s mid-retirement", pin.Path, own.GitDir))
	}
	if o := ownerDrift(pin.Path, fresh.FSMonitor, own.Owner); o != "" {
		blockers = append(blockers, o)
	}
	watchers, _, err := d.freshBlockers(ctx, pin.Path, "", rootPaths(fresh.Roots))
	if err != nil {
		return nil, err
	}
	blockers = append(blockers, watchers...)
	if len(blockers) > 0 {
		return nil, refused(blockers)
	}
	if own.Owner == nil {
		return nil, nil
	}
	if err := d.guard(ctx, pin.Path); err != nil {
		return nil, err
	}
	if err := repinWorktree(pin); err != nil {
		return nil, err
	}
	if err := d.sameOwner(ctx, *fresh.FSMonitor, fresh.Sockets); err != nil {
		return nil, err
	}
	if err := d.stopFSMonitor(ctx, fresh.GitDir, pin.Path, *fresh.FSMonitor, fresh.Sockets); err != nil {
		return nil, err
	}
	return fresh.FSMonitor, nil
}

func (d Deps) verifyNoRootsWithin(ctx context.Context, worktree string) error {
	srv, err := d.probe(ctx)
	if err != nil {
		return err
	}
	if srv.Absent != "" {
		return nil
	}
	roots, err := d.watchList(ctx)
	if err != nil {
		return err
	}
	var late []string
	for _, r := range roots {
		if under(r, worktree) {
			late = append(late, r)
		}
	}
	if len(late) > 0 {
		return fmt.Errorf("%w: a late consumer re-watched %s after retirement", ErrRefused, strings.Join(late, ", "))
	}
	return nil
}

// Recreate re-creates one exact idle root so Watchman loads its current
// .watchmanconfig, which it otherwise never rereads. It pins that file's
// identity and bytes, refuses unless the fresh gate finds no subscription,
// trigger, query, or unaccounted client, and before each mutation calls
// [Deps.Guard], re-pins the root directory and the config file, and — before
// the watch-del — checks consumers once more. Config drift before the
// watch-del refuses with nothing changed; drift after it returns the root as
// retired and never loads the unapproved file. A root whose loaded config
// already matches disk is left alone.
func Recreate(ctx context.Context, d Deps, root string) (Outcome, error) {
	if d.Guard == nil {
		return Outcome{}, ErrNoGuard
	}
	srv, err := d.probe(ctx)
	if err != nil {
		return Outcome{}, err
	}
	if srv.Absent != "" {
		return Outcome{}, refused([]string{srv.Absent})
	}
	roots, err := d.watchList(ctx)
	if err != nil {
		return Outcome{}, err
	}
	if !slices.Contains(roots, root) {
		return Outcome{}, refused([]string{root + " is not a watched root"})
	}
	pin, err := resolveTarget(root)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if pin.Path != root {
		return Outcome{}, refused([]string{fmt.Sprintf("%s now resolves to %s", root, pin.Path)})
	}
	loaded, err := d.loadedConfig(ctx, root)
	if err != nil {
		return Outcome{}, err
	}
	disk := snapshotConfig(root)
	if disk.Error != "" {
		return Outcome{}, refused([]string{root + ": " + disk.Error})
	}
	if sameConfig(loaded, disk.Raw) {
		return Outcome{}, nil
	}
	g, err := d.gate(ctx, root, true)
	if err != nil {
		return Outcome{}, err
	}
	if len(g.Blockers) > 0 {
		return Outcome{}, refused(g.Blockers)
	}
	target := WatchedRoot{Path: root, Dev: pin.Dev, Ino: pin.Ino}
	if err := d.recheckRecreation(ctx, pin, target, disk); err != nil {
		return Outcome{}, err
	}
	retired, err := d.deleteRoot(ctx, root, root, []string{root})
	if err != nil {
		return Outcome{}, err
	}
	if !retired {
		return Outcome{}, refused([]string{root + " stopped being watched before recreation"})
	}
	out := Outcome{Retired: []string{root}}
	if err := d.recheckRecreation(ctx, pin, target, disk); err != nil {
		return out, err
	}
	if err := d.watch(ctx, root); err != nil {
		return out, err
	}
	out = Outcome{Recreated: root}
	reloaded, err := d.loadedConfig(ctx, root)
	if err != nil {
		return out, err
	}
	if !sameConfig(reloaded, disk.Raw) {
		return out, fmt.Errorf("recreated %s loads a config that differs from the approved .watchmanconfig", root)
	}
	return out, nil
}

func (d Deps) recheckRecreation(ctx context.Context, pin identity, root WatchedRoot, disk configSnapshot) error {
	if err := d.guard(ctx, root.Path); err != nil {
		return err
	}
	if err := repinRoot(pin, root); err != nil {
		return err
	}
	if now := snapshotConfig(root.Path); !now.same(disk) {
		return fmt.Errorf("%w: %s/.watchmanconfig changed since recreation was planned", ErrRefused, root.Path)
	}
	return nil
}

func (d Deps) gate(ctx context.Context, target string, exact bool) (gateResult, error) {
	srv, err := d.probe(ctx)
	if err != nil {
		return gateResult{}, err
	}
	if srv.Absent != "" {
		return gateResult{}, nil
	}
	status, err := d.debugStatus(ctx)
	if err != nil {
		return gateResult{}, err
	}
	status.ServerPID = srv.PID
	var g gateResult
	for _, raw := range status.Roots {
		switch {
		case raw.Path == target || (!exact && under(raw.Path, target)):
			g.Roots = append(g.Roots, raw.Path)
			c, err := d.consumers(ctx, raw.Path)
			if err != nil {
				return gateResult{}, err
			}
			g.Blockers = append(g.Blockers, d.blockers(raw.Path, raw.Queries, c)...)
		case under(target, raw.Path):
			cov, blockers, err := d.ancestor(ctx, raw, target)
			if err != nil {
				return gateResult{}, err
			}
			g.Ancestors = append(g.Ancestors, cov)
			g.Blockers = append(g.Blockers, blockers...)
		}
	}
	slices.SortStableFunc(g.Roots, func(a, b string) int { return len(b) - len(a) })
	clientBlockers, err := d.clientBlockers(ctx, status, g.Roots)
	if err != nil {
		return gateResult{}, err
	}
	g.Blockers = append(g.Blockers, clientBlockers...)
	return g, nil
}

func (d Deps) ancestor(ctx context.Context, raw rawRoot, target string) (Coverage, []string, error) {
	config, err := d.loadedConfig(ctx, raw.Path)
	if err != nil {
		return Coverage{}, nil, err
	}
	cov := Coverage{Root: raw.Path, Ignored: ignoredWithin(target, raw.Path, ignoreDirs(config))}
	if cov.Ignored {
		return cov, nil, nil
	}
	c, err := d.consumers(ctx, raw.Path)
	if err != nil {
		return Coverage{}, nil, err
	}
	var blockers []string
	for _, b := range d.blockers(raw.Path, raw.Queries, c) {
		blockers = append(blockers, "ancestor root does not ignore "+target+": "+b)
	}
	return cov, blockers, nil
}

func (d Deps) freshBlockers(ctx context.Context, worktree, root string, targets []string) ([]string, bool, error) {
	srv, err := d.probe(ctx)
	if err != nil {
		return nil, false, err
	}
	if srv.Absent != "" {
		return nil, false, nil
	}
	status, err := d.debugStatus(ctx)
	if err != nil {
		return nil, false, err
	}
	status.ServerPID = srv.PID
	var blockers []string
	watched := false
	for _, raw := range status.Roots {
		switch {
		case root != "" && raw.Path == root:
			watched = true
			c, err := d.consumers(ctx, root)
			if err != nil {
				return nil, false, err
			}
			blockers = append(blockers, d.blockers(root, raw.Queries, c)...)
		case !under(raw.Path, worktree) && under(worktree, raw.Path):
			_, ab, err := d.ancestor(ctx, raw, worktree)
			if err != nil {
				return nil, false, err
			}
			blockers = append(blockers, ab...)
		}
	}
	clientBlockers, err := d.clientBlockers(ctx, status, targets)
	if err != nil {
		return nil, false, err
	}
	return append(blockers, clientBlockers...), watched, nil
}

func (d Deps) blockers(root string, queries []rawQuery, c rootConsumers) []string {
	b := make([]string, 0, len(queries)+len(c.Subscriptions)+len(c.Triggers))
	for _, q := range queries {
		b = append(b, fmt.Sprintf("query in flight on %s (pid %d, %s)", root, q.ClientPID, q.State))
	}
	for _, s := range c.Subscriptions {
		b = append(b, fmt.Sprintf("subscription %q (pid %d) on %s", s.Name, s.PID, root))
	}
	for _, t := range c.Triggers {
		kind := "trigger"
		if d.protected(strings.Join(t.Command, " ")) {
			kind = "protected trigger"
		}
		b = append(b, fmt.Sprintf("%s %q on %s", kind, t.Name, root))
	}
	return b
}

type placement struct {
	Roots   []string
	Clients map[string]bool
}

func (d Deps) clientBlockers(ctx context.Context, status watchmanStatus, targets []string) ([]string, error) {
	clients, err := d.inspectClients(ctx, status)
	if err != nil {
		return nil, err
	}
	var foreign []Client
	var blockers []string
	unexamined := 0
	conns := map[int]int{}
	for _, c := range clients {
		switch {
		case c.Self || c.Server || c.Gone:
			continue
		case c.Unexamined:
			unexamined++
			continue
		}
		if conns[c.PID]++; conns[c.PID] == 1 {
			foreign = append(foreign, c)
		}
	}
	if unexamined > 0 {
		blockers = append(blockers, fmt.Sprintf("%d watchman connections past the bound of %d client pids were not inspected", unexamined, d.MaxClients))
	}
	if len(foreign) == 0 {
		return blockers, nil
	}
	placed, bounded, err := d.placements(ctx, status.Roots)
	if err != nil {
		return nil, err
	}
	for _, c := range foreign {
		p := placed[c.PID]
		label := "unaccounted watchman client"
		if c.Protected {
			label = "protected consumer"
		}
		switch {
		case c.PID == 0:
			blockers = append(blockers, fmt.Sprintf("watchman client with no peer pid (%s)", c.State))
		case len(p.Roots) == 0 && bounded:
			blockers = append(blockers, fmt.Sprintf("%s pid %d (%s) connected; census bounded at %d roots cannot place it", label, c.PID, c.Command, d.MaxRoots))
		case len(p.Roots) == 0:
			blockers = append(blockers, fmt.Sprintf("%s pid %d (%s) connected with no subscription placing it", label, c.PID, c.Command))
		case conns[c.PID] > len(p.Clients):
			blockers = append(blockers, fmt.Sprintf("%s pid %d (%s) holds %d connections but subscriptions place only %d", label, c.PID, c.Command, conns[c.PID], len(p.Clients)))
		case slices.ContainsFunc(p.Roots, func(r string) bool { return slices.Contains(targets, r) }):
			blockers = append(blockers, fmt.Sprintf("%s pid %d (%s) subscribes to %s", label, c.PID, c.Command, strings.Join(p.Roots, ", ")))
		}
	}
	return blockers, nil
}

func (d Deps) placements(ctx context.Context, roots []rawRoot) (map[int]placement, bool, error) {
	bounded := len(roots) > d.MaxRoots
	if bounded {
		roots = roots[:d.MaxRoots]
	}
	found := make([][]Subscription, len(roots))
	var g errgroup.Group
	g.SetLimit(censusWidth)
	for i, raw := range roots {
		g.Go(func() error {
			c, err := d.consumers(ctx, raw.Path)
			var werr *WatchmanError
			if errors.As(err, &werr) {
				return nil
			}
			found[i] = c.Subscriptions
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, false, err
	}
	placed := map[int]placement{}
	for i, subs := range found {
		for _, s := range subs {
			if s.PID == 0 {
				continue
			}
			p := placed[s.PID]
			if p.Clients == nil {
				p.Clients = map[string]bool{}
			}
			p.Roots = append(p.Roots, roots[i].Path)
			p.Clients[s.Client] = true
			placed[s.PID] = p
		}
	}
	return placed, bounded, nil
}
