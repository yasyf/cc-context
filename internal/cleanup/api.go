package cleanup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Protocol is the version of the daemon's wire protocol. A client and a daemon
// that disagree on it refuse each other rather than guess.
const Protocol = 1

var (
	// ErrUnsupported reports a platform the daemon does not run on; removal
	// there stays inline.
	ErrUnsupported = errors.New("cleanup: the deletion daemon runs on macOS only")
	// ErrUnknownJob reports a job id the journal does not hold.
	ErrUnknownJob = errors.New("cleanup: no such job")
	// ErrIdentity reports a file that is not the one a job captured.
	ErrIdentity = errors.New("cleanup: identity mismatch")
	// ErrUnprobed reports a process or watcher probe that timed out or could
	// not read a process, so what holds a tree could be neither named nor
	// ruled out.
	ErrUnprobed = errors.New("cleanup: a process or watcher probe failed")
)

// Request asks for one registered linked worktree to be removed now.
type Request struct {
	// Worktree is the absolute path the worktree is registered at.
	Worktree string `json:"worktree"`
	// Force discards uncommitted changes. It overrides no other refusal.
	Force bool `json:"force,omitempty"`
	// Git is the absolute git binary the caller resolved.
	Git string `json:"git"`
}

// Validate rejects a request whose paths are not clean and absolute.
func (r Request) Validate() error {
	return validatePaths(map[string]string{"worktree": r.Worktree, "git": r.Git})
}

// Registration is the identity of one linked worktree as git registers it: the
// directory at its root and the admin directory that names it. A different
// worktree at the same path shares neither.
type Registration struct {
	// Tree is the identity of the worktree's root directory.
	Tree FileID `json:"tree"`
	// AdminDir is the symlink-resolved <common>/worktrees/<name> directory.
	AdminDir string `json:"admin_dir"`
	// Admin is the identity of AdminDir.
	Admin FileID `json:"admin"`
}

// Validate rejects a registration that leaves either identity unset or names
// its admin directory by anything but a clean absolute path, so the zero value
// never validates.
func (r Registration) Validate() error {
	if r.Tree.Zero() {
		return errors.New("tree identity is unset")
	}
	if r.Admin.Zero() {
		return errors.New("admin identity is unset")
	}
	return validatePaths(map[string]string{"admin_dir": r.AdminDir})
}

// DeferRequest records a ccx-owned workspace for removal once nothing holds
// it. It never refuses for activity: an occupied workspace waits. It removes
// only the worktree Expected names: a path that has come to hold any other
// registration is refused.
type DeferRequest struct {
	Worktree string `json:"worktree"`
	// CommonDir is the git common directory the caller believes the workspace
	// belongs to; a workspace of any other repository is refused.
	CommonDir string `json:"common_dir"`
	// Owner names the ccx feature that created the workspace.
	Owner string `json:"owner"`
	// Expected is the registration the caller observed at Worktree while it
	// still knew the workspace to be its own.
	Expected Registration `json:"expected"`
	Force    bool         `json:"force,omitempty"`
	Git      string       `json:"git"`
}

// Validate rejects a request whose paths are not clean and absolute, whose
// owner is unnamed, or whose expected registration is unset.
func (r DeferRequest) Validate() error {
	if strings.TrimSpace(r.Owner) == "" {
		return errors.New("owner is empty")
	}
	if err := validatePaths(map[string]string{"worktree": r.Worktree, "common_dir": r.CommonDir, "git": r.Git}); err != nil {
		return err
	}
	if err := r.Expected.Validate(); err != nil {
		return fmt.Errorf("expected: %w", err)
	}
	return nil
}

const (
	// LegacyRecoveryPrefix is the ref namespace the retired janitor pinned each
	// quarantined worktree's head under, as <prefix><date>/<id>.
	LegacyRecoveryPrefix = "refs/cleanup-worktrees/"
	// LegacyQuarantinePrefix names the retired janitor's quarantine directory,
	// <prefix><date>, holding one <id>-<name> tree per pinned head.
	LegacyQuarantinePrefix = "codex-worktree-trash-"
)

var legacyRefPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(LegacyRecoveryPrefix) + `(\d{8})/([0-9a-f]{20})$`)

// LegacyBinding reports whether source is the quarantine path the retired
// janitor's naming scheme assigns to ref: <any>/codex-worktree-trash-<date>/<id>-<name>
// for refs/cleanup-worktrees/<date>/<id>. Adoption accepts no other pairing, so
// a request can name only a tree that scheme parked and the one ref that pins
// it.
func LegacyBinding(source, ref string) error {
	m := legacyRefPattern.FindStringSubmatch(ref)
	if m == nil {
		return fmt.Errorf("recovery_ref %q is not %s<date>/<id>", ref, LegacyRecoveryPrefix)
	}
	date, id := m[1], m[2]
	if filepath.Base(filepath.Dir(source)) != LegacyQuarantinePrefix+date {
		return fmt.Errorf("source %q is not inside a %s%s quarantine", source, LegacyQuarantinePrefix, date)
	}
	if base := filepath.Base(source); !strings.HasPrefix(base, id+"-") || len(base) == len(id)+1 {
		return fmt.Errorf("source %q is not the tree %s pins", source, ref)
	}
	return nil
}

// AdoptRequest hands the daemon one tree the retired janitor already
// unregistered and parked in its quarantine. It is the operator's one-time
// import path, not a way to delete a directory by name: the daemon takes only
// the exact inode the request names, only from the quarantine path the legacy
// naming scheme binds to the ref, and only while that ref still resolves to
// the head the manifest saved.
type AdoptRequest struct {
	// Source is the canonical absolute path the tree is parked at.
	Source string `json:"source"`
	// Tree is the identity the operator validated for Source.
	Tree FileID `json:"tree"`
	// CommonDir is the git common directory the tree was a worktree of.
	CommonDir string `json:"common_dir"`
	// Head is the commit the manifest saved for the tree.
	Head string `json:"head"`
	// RecoveryRef is the legacy ref in CommonDir that must resolve to Head.
	RecoveryRef string `json:"recovery_ref"`
	// Original is where the worktree used to be registered.
	Original string `json:"original"`
	Owner    string `json:"owner"`
	Git      string `json:"git"`
}

// Validate rejects a request that does not name one quarantined tree, its
// identity, and the legacy ref bound to it.
func (r AdoptRequest) Validate() error {
	if strings.TrimSpace(r.Owner) == "" {
		return errors.New("owner is empty")
	}
	if r.Tree.Zero() {
		return errors.New("tree identity is unset")
	}
	if !oidPattern.MatchString(r.Head) {
		return fmt.Errorf("head %q is not an object id", r.Head)
	}
	if err := validatePaths(map[string]string{
		"source": r.Source, "common_dir": r.CommonDir, "original": r.Original, "git": r.Git,
	}); err != nil {
		return err
	}
	return LegacyBinding(r.Source, r.RecoveryRef)
}

func validatePaths(named map[string]string) error {
	for name, path := range named {
		if !cleanAbs(path) {
			return fmt.Errorf("%s %q is not a clean absolute path", name, path)
		}
	}
	return nil
}

// ProcessID is one process's exact identity: a pid and the instant the kernel
// started it, so a recycled pid never matches.
type ProcessID struct {
	PID int `json:"pid"`
	// Start is the process start time in Unix seconds.
	Start int64 `json:"start"`
}

type retiringKey struct{}

// WithRetiring returns ctx naming the watcher processes about to be retired
// from the tree the work under ctx examines. A guard discounts the
// descriptors those exact processes hold, and nothing else about them.
func WithRetiring(ctx context.Context, watchers []ProcessID) context.Context {
	return context.WithValue(ctx, retiringKey{}, watchers)
}

// RetiringFrom returns the watcher processes ctx names.
func RetiringFrom(ctx context.Context) []ProcessID {
	watchers, _ := ctx.Value(retiringKey{}).([]ProcessID)
	return watchers
}

type requesterKey struct{}

// WithRequester returns ctx naming pid as the transient client whose request
// the work under ctx serves. A guard may discount that one process naming the
// tree in its own arguments, and nothing else about it.
func WithRequester(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, requesterKey{}, pid)
}

// RequesterFrom returns the transient client ctx names, if any.
func RequesterFrom(ctx context.Context) (int, bool) {
	pid, ok := ctx.Value(requesterKey{}).(int)
	return pid, ok
}

// Receipt is what a caller holds once the daemon has accepted a removal.
type Receipt struct {
	JobID       string `json:"job_id"`
	State       State  `json:"state"`
	Original    string `json:"original"`
	RecoveryRef string `json:"recovery_ref,omitempty"`
	// Detail says what a waiting job waits on.
	Detail string `json:"detail,omitempty"`
}

// ReceiptOf is the receipt for job as it stands.
func ReceiptOf(job Job) Receipt {
	return Receipt{JobID: job.ID, State: job.State(), Original: job.Original, RecoveryRef: job.RecoveryRef}
}

// Query selects what a status report carries.
type Query struct {
	// JobID narrows the report to one job; empty reports the queue.
	JobID string `json:"job_id,omitempty"`
	// Limit caps the jobs reported; zero takes the daemon's default.
	Limit int `json:"limit,omitempty"`
}

// Governor is the deletion throttle as an observer reads it.
type Governor struct {
	// State is idle, sampling, clear, throttled, or unavailable.
	State string `json:"state"`
	// CPUPercent is fseventsd's last sampled CPU use, 100 being one core.
	CPUPercent float64 `json:"cpu_percent"`
	Detail     string  `json:"detail,omitempty"`
}

// Report is the daemon's bounded view of its queue: unfinished jobs first in
// queue order, then the most recently finished, with no total file count and
// no estimate of time remaining.
type Report struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Paused reports that physical deletion is stopped queue-wide.
	Paused   bool     `json:"paused"`
	Governor Governor `json:"governor"`
	Jobs     []Job    `json:"jobs"`
	// Omitted counts the jobs the limit left out.
	Omitted int `json:"omitted,omitempty"`
	// Damaged names job folders whose record could not be read; the daemon
	// leaves them untouched.
	Damaged []Damaged `json:"damaged,omitempty"`
}

// Info identifies a serving daemon to the client deciding whether to replace
// it.
type Info struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
}

// Service is the daemon as a caller uses it, in process or over its socket.
type Service interface {
	// Remove relocates and unregisters a worktree and returns once the removal
	// is logically complete. A preflight refusal is a *RefusedError or an
	// *ActiveError with nothing journaled; a removal that stopped part-way is a
	// *BlockedError.
	Remove(ctx context.Context, r Request) (Receipt, error)
	// Defer journals a removal that waits for inactivity and returns without
	// moving anything.
	Defer(ctx context.Context, r DeferRequest) (Receipt, error)
	// Status reports the queue, or one job.
	Status(ctx context.Context, q Query) (Report, error)
	// Wait returns once the job is done, or with a *BlockedError once it
	// blocks.
	Wait(ctx context.Context, jobID string) (Job, error)
	// Pause stops physical deletion queue-wide. Logical removals still run,
	// and every job stays durable.
	Pause(ctx context.Context) error
	// Resume lets physical deletion continue.
	Resume(ctx context.Context) error
	// Retry clears a job's blockage and lets it run again from the phase it
	// stopped in.
	Retry(ctx context.Context, jobID string) (Job, error)
}

// Adopter is the operator's import path for the trees the retired janitor
// left in its quarantine.
type Adopter interface {
	// Adopt verifies the parked tree and journals it, then relocates it into
	// its job folder and queues its deletion. A refusal is a *RefusedError or
	// an *ActiveError with nothing journaled and nothing moved.
	Adopt(ctx context.Context, r AdoptRequest) (Receipt, error)
}

// Control is a Service reached over the daemon's socket, with the two verbs a
// client needs to replace the daemon behind it.
type Control interface {
	Service
	// Hello identifies the serving daemon.
	Hello(ctx context.Context) (Info, error)
	// Shutdown asks the daemon to finish its current bounded step, flush its
	// journal, and exit, returning once it has stopped serving.
	Shutdown(ctx context.Context) error
}

// RefusedError is a removal the preflight refused before anything was
// journaled or mutated.
type RefusedError struct {
	Worktree string `json:"worktree"`
	// Reason is a short stable category: main, unregistered, registered,
	// locked, submodules, nested, dirty, volume, mismatch, replaced, identity,
	// recovery, watched, or waiting when the tree's deferred removal still
	// waits.
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Worktree, e.Detail)
}

// Evidence kinds a guard reports for a holder.
const (
	// EvidenceCwd is a working directory at or below the tree.
	EvidenceCwd = "cwd"
	// EvidenceFD is an open file or directory at or below the tree.
	EvidenceFD = "fd"
	// EvidenceArgv is an argument naming a path at or below the tree.
	EvidenceArgv = "argv"
)

// Holder is one live process holding a tree, and the first piece of evidence
// that ties it there.
type Holder struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	// TTY reports a controlling terminal.
	TTY bool `json:"tty,omitempty"`
	// Evidence is EvidenceCwd, EvidenceFD, or EvidenceArgv.
	Evidence string `json:"evidence"`
	// Path is what the evidence names: the working directory, the open file,
	// or the argument.
	Path string `json:"path"`
}

// ActiveError is a guard's positive verdict: live processes hold the tree.
type ActiveError struct {
	Worktree string   `json:"worktree"`
	Holders  []Holder `json:"holders"`
}

func (e *ActiveError) Error() string {
	const shown = 5
	parts := make([]string, 0, shown+1)
	for i, h := range e.Holders {
		if i == shown {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Holders)-shown))
			break
		}
		tty := ""
		if h.TTY {
			tty = " on a terminal"
		}
		held := map[string]string{
			EvidenceCwd:  "working in ",
			EvidenceFD:   "holding open ",
			EvidenceArgv: "started with ",
		}[h.Evidence] + h.Path
		parts = append(parts, fmt.Sprintf("%s (pid %d%s) %s", h.Name, h.PID, tty, held))
	}
	return fmt.Sprintf("%s is in use: %s", e.Worktree, strings.Join(parts, "; "))
}

// BlockedError is a job that stopped for an operator.
type BlockedError struct {
	Job Job `json:"job"`
}

func (e *BlockedError) Error() string {
	b := e.Job.Blocked
	return fmt.Sprintf("cleanup job %s is blocked at %s (%s): %s", e.Job.ID, e.Job.Phase, b.Reason, b.Detail)
}
