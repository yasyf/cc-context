package cleanupwatch

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git names its hashed fsmonitor socket with SHA-1 of the worktree path
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	ipcSocket    = "fsmonitor--daemon.ipc"
	hashedPrefix = ".git-fsmonitor-"
)

// Daemon is one builtin Git fsmonitor daemon, identified by the IPC socket it
// listens on and the Git metadata that socket lives in — never by its working
// directory, which Git sets to the home directory for every daemon.
type Daemon struct {
	PID     int    `json:"pid"`
	Start   string `json:"start"`
	Command string `json:"command"`
	CWD     string `json:"cwd,omitempty"`
	Socket  string `json:"socket,omitempty"`
	// AdminDir is the Git directory holding Socket: a linked worktree's
	// .git/worktrees/<name>, or a main worktree's .git.
	AdminDir string `json:"admin_dir,omitempty"`
	// Worktree is the checkout AdminDir's metadata names; empty when neither
	// the socket nor the metadata names one.
	Worktree string `json:"worktree,omitempty"`
	// Orphan reports that Socket or Worktree is gone from disk.
	Orphan  bool          `json:"orphan,omitempty"`
	Config  []ConfigValue `json:"config,omitempty"`
	Error   string        `json:"error,omitempty"`
	Verdict string        `json:"verdict"`
}

// ConfigValue is one core.fsmonitor setting as Git resolves it, in precedence
// order: the last entry is the effective one.
type ConfigValue struct {
	Scope  string `json:"scope"`
	Origin string `json:"origin"`
	Value  string `json:"value"`
}

func (d Deps) daemons(ctx context.Context) ([]Daemon, error) {
	procs, err := d.Procs.FSMonitorDaemons(ctx)
	if err != nil {
		return nil, err
	}
	daemons := make([]Daemon, 0, len(procs))
	for _, p := range procs {
		daemons = append(daemons, identify(p))
	}
	return daemons, nil
}

func (d Deps) resolvedDaemons(ctx context.Context) ([]Daemon, error) {
	daemons, err := d.daemons(ctx)
	if err != nil {
		return nil, err
	}
	for i, dm := range daemons {
		if dm.Verdict != VerdictRetirable {
			continue
		}
		gitDir, top, err := d.revParse(ctx, dm.Worktree)
		var exitErr *ExitError
		switch {
		case errors.As(err, &exitErr):
			daemons[i].Error, daemons[i].Verdict = fmt.Sprintf("git cannot resolve %s: %s", dm.Worktree, strings.TrimSpace(exitErr.Stderr)), VerdictUnresolved
		case err != nil:
			return nil, err
		case !sameFile(gitDir, dm.AdminDir) || !sameFile(top, dm.Worktree):
			daemons[i].Error, daemons[i].Verdict = fmt.Sprintf("git resolves %s to git dir %s and worktree %s", dm.Worktree, gitDir, top), VerdictUnresolved
		}
	}
	return daemons, nil
}

func (d Deps) revParse(ctx context.Context, dir string) (string, string, error) {
	out, err := d.run(ctx, "git", "-C", dir, "rev-parse", "--absolute-git-dir", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("git rev-parse in %s: %q, want git dir and toplevel", dir, out)
	}
	return lines[0], lines[1], nil
}

func identify(p Process) Daemon {
	dm := Daemon{PID: p.PID, Start: p.Start, Command: p.Command, CWD: p.CWD}
	for _, s := range p.Sockets {
		if base := filepath.Base(s); base == ipcSocket || strings.HasPrefix(base, hashedPrefix) {
			dm.Socket = s
			break
		}
	}
	unresolved := func(reason string) Daemon {
		dm.Error, dm.Verdict = reason, VerdictUnresolved
		return dm
	}
	switch {
	case p.Unexamined:
		return unresolved("past the process table's bound on fsmonitor daemons; not inspected")
	case dm.Socket == "":
		return unresolved("no fsmonitor IPC socket among its open files")
	case !filepath.IsAbs(dm.Socket):
		return unresolved("relative IPC socket " + dm.Socket)
	}
	dm.Socket = canonicalSocket(dm.Socket)
	if _, err := os.Lstat(dm.Socket); errors.Is(err, fs.ErrNotExist) {
		dm.Orphan, dm.Verdict = true, VerdictOrphan
		return dm
	} else if err != nil {
		return unresolved(err.Error())
	}
	if filepath.Base(dm.Socket) != ipcSocket {
		return unresolved("hashed IPC socket names no worktree")
	}
	dm.AdminDir = filepath.Dir(dm.Socket)
	wt, err := worktreeOf(dm.AdminDir)
	if err != nil {
		return unresolved(err.Error())
	}
	dm.Worktree = canonicalOrSelf(wt)
	if _, err := os.Stat(dm.Worktree); errors.Is(err, fs.ErrNotExist) {
		dm.Orphan, dm.Verdict = true, VerdictOrphan
		return dm
	} else if err != nil {
		return unresolved(err.Error())
	}
	dm.Verdict = VerdictRetirable
	return dm
}

func worktreeOf(admin string) (string, error) {
	b, err := os.ReadFile(filepath.Join(admin, "gitdir")) //nolint:gosec // admin is a Git directory from git rev-parse or a live daemon's IPC socket; gitdir is Git's fixed back-link in it
	if errors.Is(err, fs.ErrNotExist) {
		if filepath.Base(admin) != ".git" {
			return "", fmt.Errorf("%s: no gitdir link and not a .git directory", admin)
		}
		return filepath.Dir(admin), nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s gitdir link: %w", admin, err)
	}
	link := strings.TrimSpace(string(b))
	if !filepath.IsAbs(link) {
		link = filepath.Join(admin, link)
	}
	if filepath.Base(link) != ".git" {
		return "", fmt.Errorf("%s gitdir link %q does not name a .git file", admin, link)
	}
	return filepath.Dir(filepath.Clean(link)), nil
}

func (d Deps) fsmonitorConfig(ctx context.Context, worktree string) ([]ConfigValue, error) {
	out, err := d.run(ctx, "git", "-C", worktree, "config", "-z", "--show-scope", "--show-origin", "--get-all", "core.fsmonitor")
	if exitCode(err) == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("core.fsmonitor origin in %s: %w", worktree, err)
	}
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("core.fsmonitor origin in %s: %d fields, want triples", worktree, len(fields))
	}
	values := make([]ConfigValue, 0, len(fields)/3)
	for i := 0; i < len(fields); i += 3 {
		values = append(values, ConfigValue{Scope: string(fields[i]), Origin: string(fields[i+1]), Value: string(fields[i+2])})
	}
	return values, nil
}

type ownership struct {
	GitDir   string
	Sockets  []string
	Owner    *Daemon
	Blockers []string
}

func (d Deps) fsmonitorOwnership(ctx context.Context, worktree string) (ownership, error) {
	var own ownership
	gitDir, top, err := d.revParse(ctx, worktree)
	var exitErr *ExitError
	switch {
	case errors.As(err, &exitErr):
		own.Blockers = append(own.Blockers, fmt.Sprintf("git cannot resolve %s: %s", worktree, strings.TrimSpace(exitErr.Stderr)))
	case err != nil:
		return ownership{}, fmt.Errorf("git dir of %s: %w", worktree, err)
	default:
		own.GitDir = canonicalOrSelf(gitDir)
		if !sameFile(top, worktree) {
			own.Blockers = append(own.Blockers, fmt.Sprintf("git resolves %s to worktree %s", worktree, top))
		}
		linked, err := worktreeOf(own.GitDir)
		switch {
		case err != nil:
			own.Blockers = append(own.Blockers, fmt.Sprintf("git metadata of %s: %v", worktree, err))
		case !sameFile(linked, worktree):
			own.Blockers = append(own.Blockers, fmt.Sprintf("git dir %s links back to %s, not %s", own.GitDir, linked, worktree))
		}
		own.Sockets, err = d.expectedSockets(ctx, worktree, own.GitDir)
		if err != nil {
			return ownership{}, err
		}
	}
	daemons, err := d.resolvedDaemons(ctx)
	if err != nil {
		return ownership{}, err
	}
	for _, dm := range daemons {
		switch {
		case ownsSocket(own.Sockets, dm) && dm.Orphan:
			own.Blockers = append(own.Blockers, fmt.Sprintf("fsmonitor daemon %d serves %s through socket %s, which no longer exists", dm.PID, worktree, dm.Socket))
		case ownsSocket(own.Sockets, dm):
			if own.Owner != nil {
				own.Blockers = append(own.Blockers, fmt.Sprintf("fsmonitor daemons %d and %d both own %s", own.Owner.PID, dm.PID, dm.Socket))
				continue
			}
			owner := dm
			own.Owner = &owner
		case dm.Verdict == VerdictUnresolved:
			own.Blockers = append(own.Blockers, fmt.Sprintf("fsmonitor daemon %d has an unresolved worktree: %s", dm.PID, dm.Error))
		case dm.Worktree == "":
			own.Blockers = append(own.Blockers, fmt.Sprintf("fsmonitor daemon %d lost its socket %s and names no worktree", dm.PID, dm.Socket))
		case under(dm.Worktree, worktree):
			own.Blockers = append(own.Blockers, fmt.Sprintf("fsmonitor daemon %d watches %s inside %s without an exact socket match", dm.PID, dm.Worktree, worktree))
		case dm.CWD != "" && under(dm.CWD, worktree):
			own.Blockers = append(own.Blockers, fmt.Sprintf("fsmonitor daemon %d runs inside %s without owning its socket", dm.PID, worktree))
		}
	}
	if own.Owner != nil {
		config, err := d.fsmonitorConfig(ctx, worktree)
		if err != nil {
			return ownership{}, err
		}
		own.Owner.Config = config
	}
	return own, nil
}

func ownsSocket(sockets []string, dm Daemon) bool {
	return dm.Socket != "" && slices.ContainsFunc(sockets, func(s string) bool { return sameFile(s, dm.Socket) })
}

func (d Deps) expectedSockets(ctx context.Context, worktree, gitDir string) ([]string, error) {
	dir, err := d.socketDir(ctx, worktree)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(gitDir, ipcSocket), filepath.Join(canonicalOrSelf(dir), hashedSocket(worktree))}, nil
}

func (d Deps) socketDir(ctx context.Context, worktree string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	out, err := d.run(ctx, "git", "-C", worktree, "config", "--get", "fsmonitor.socketDir")
	if exitCode(err) == 1 {
		return home, nil
	}
	if err != nil {
		return "", fmt.Errorf("fsmonitor.socketDir in %s: %w", worktree, err)
	}
	dir := strings.TrimSpace(string(out))
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		return filepath.Join(home, rest), nil
	}
	return dir, nil
}

func hashedSocket(worktree string) string {
	sum := sha1.Sum([]byte(worktree)) //nolint:gosec // matches git's own socket naming, not a security boundary
	return hashedPrefix + hex.EncodeToString(sum[:])
}

func (d Deps) daemonGit(ctx context.Context, gitDir, worktree, verb string) error {
	_, err := d.run(ctx, "git", "--git-dir="+gitDir, "--work-tree="+worktree, "fsmonitor--daemon", verb)
	return err
}

func (d Deps) sameOwner(ctx context.Context, planned Daemon, sockets []string) error {
	daemons, err := d.daemons(ctx)
	if err != nil {
		return err
	}
	var owners []Daemon
	var unresolved []string
	for _, dm := range daemons {
		switch {
		case dm.PID == planned.PID || ownsSocket(sockets, dm):
			owners = append(owners, dm)
		case dm.Verdict == VerdictUnresolved:
			unresolved = append(unresolved, fmt.Sprintf("daemon %d (%s)", dm.PID, dm.Error))
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("%w: fsmonitor ownership of %s is unconfirmed after the activity check: %s", ErrRefused, planned.Socket, strings.Join(unresolved, "; "))
	}
	if len(owners) != 1 || !sameProcess(owners[0], planned) || owners[0].Socket != planned.Socket {
		return fmt.Errorf("%w: fsmonitor owner changed during the activity check: planned %s, found %s", ErrRefused, describe(planned), describeAll(owners))
	}
	return nil
}

func sameProcess(a, b Daemon) bool {
	return a.PID == b.PID && a.Start == b.Start && a.Command == b.Command
}

func describe(dm Daemon) string {
	return fmt.Sprintf("pid %d started %s (%s) on %s", dm.PID, dm.Start, dm.Command, dm.Socket)
}

func describeAll(daemons []Daemon) string {
	if len(daemons) == 0 {
		return "none"
	}
	parts := make([]string, len(daemons))
	for i, dm := range daemons {
		parts[i] = describe(dm)
	}
	return strings.Join(parts, "; ")
}

func (d Deps) stopFSMonitor(ctx context.Context, gitDir, worktree string, owner Daemon, sockets []string) error {
	if err := d.daemonGit(ctx, gitDir, worktree, "stop"); err != nil && exitCode(err) != 1 {
		return fmt.Errorf("stop fsmonitor daemon %d: %w", owner.PID, err)
	}
	for range d.SettleTries {
		alive, err := d.daemonAlive(ctx, owner.PID, sockets)
		if err != nil {
			return err
		}
		if !alive {
			return d.verifyNotWatching(ctx, gitDir, worktree)
		}
		if err := sleep(ctx, d.SettleInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("fsmonitor daemon %d still serves %s after stop", owner.PID, worktree)
}

func (d Deps) daemonAlive(ctx context.Context, pid int, sockets []string) (bool, error) {
	daemons, err := d.daemons(ctx)
	if err != nil {
		return false, err
	}
	for _, dm := range daemons {
		if dm.PID == pid || ownsSocket(sockets, dm) {
			return true, nil
		}
	}
	return false, nil
}

func (d Deps) verifyNotWatching(ctx context.Context, gitDir, worktree string) error {
	err := d.daemonGit(ctx, gitDir, worktree, "status")
	switch {
	case exitCode(err) == 1:
		return nil
	case err != nil:
		return fmt.Errorf("fsmonitor status in %s: %w", worktree, err)
	}
	return fmt.Errorf("%w: an fsmonitor daemon is watching %s again after stop", ErrRefused, worktree)
}

func exitCode(err error) int {
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return 0
}
