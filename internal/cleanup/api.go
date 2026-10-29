package cleanup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Protocol is the version of the daemon's wire protocol. A client and a daemon
// that disagree on it refuse each other rather than guess. Both sides decode
// strictly, so any change to a request or reply shape bumps it.
const Protocol = 5

var (
	// ErrUnsupported reports a platform the daemon does not run on; removal
	// there stays inline.
	ErrUnsupported = errors.New("cleanup: the deletion daemon runs on macOS only")
	// ErrUnknownJob reports a job id the journal does not hold.
	ErrUnknownJob = errors.New("cleanup: no such job")
	// ErrPaused reports a paused queue, which refuses every new deferral or
	// adoption before any preflight, journal record, git call, or filesystem
	// change.
	ErrPaused = errors.New("cleanup: the queue is paused and takes no new deferral or adoption until ccx vcs cleanup resume")
	// ErrIdentity reports a file that is not the one a job captured.
	ErrIdentity = errors.New("cleanup: identity mismatch")
	// ErrUnprobed reports a process, watcher, or git probe that timed out or
	// could not read a process, so what holds a tree could be neither named
	// nor ruled out.
	ErrUnprobed = errors.New("cleanup: a process or watcher probe failed")
	// ErrUnidentifiedPeer reports a daemon connection whose serving process the
	// kernel would not name, so nothing was sent on it.
	ErrUnidentifiedPeer = errors.New("cleanup: the process serving the daemon socket could not be identified")
	// ErrNotHeld reports a Release while no maintenance hold stands.
	ErrNotHeld = errors.New("cleanup: no maintenance hold stands on the queue")
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

// Ignored is a process a guard passed over although the kernel would not say
// where each of its descriptors points, with the reason it cannot be holding
// the tree.
type Ignored struct {
	PID    int    `json:"pid"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func (i Ignored) String() string {
	return fmt.Sprintf("%s (pid %d, %s)", i.Name, i.PID, i.Reason)
}

type ignoredKey struct{}

// WithIgnored returns ctx carrying an empty list, and that list, which every
// guard run under ctx extends with the processes it passed over.
func WithIgnored(ctx context.Context) (context.Context, *[]Ignored) {
	var ignored []Ignored
	return context.WithValue(ctx, ignoredKey{}, &ignored), &ignored
}

// NoteIgnored extends the list ctx carries with ignored, when ctx carries one.
func NoteIgnored(ctx context.Context, ignored ...Ignored) {
	if list, ok := ctx.Value(ignoredKey{}).(*[]Ignored); ok {
		*list = append(*list, ignored...)
	}
}

type requesterKey struct{}

// WithRequester returns ctx naming the transient client whose request the work
// under ctx serves. While that exact process lives, a guard may discount it
// naming the tree in its own arguments, and the same arguments carried by the
// wrappers that launched it, and nothing else about them.
func WithRequester(ctx context.Context, requester ProcessID) context.Context {
	return context.WithValue(ctx, requesterKey{}, requester)
}

// RequesterFrom returns the transient client ctx names, if any.
func RequesterFrom(ctx context.Context) (ProcessID, bool) {
	requester, ok := ctx.Value(requesterKey{}).(ProcessID)
	return requester, ok
}

// Receipt is what a caller holds once the daemon has accepted a removal.
type Receipt struct {
	JobID       string `json:"job_id"`
	State       State  `json:"state"`
	Original    string `json:"original"`
	RecoveryRef string `json:"recovery_ref,omitempty"`
	// Detail says what a waiting job waits on.
	Detail string `json:"detail,omitempty"`
	// Ignored names the processes the job's last activity check passed over,
	// each with the reason it could not be holding the tree.
	Ignored string `json:"ignored,omitempty"`
}

// ReceiptOf is the receipt for job as it stands.
func ReceiptOf(job Job) Receipt {
	ignored := make([]string, len(job.Ignored))
	for i, process := range job.Ignored {
		ignored[i] = process.String()
	}
	return Receipt{
		JobID: job.ID, State: job.State(), Original: job.Original, RecoveryRef: job.RecoveryRef,
		Ignored: strings.Join(ignored, "; "),
	}
}

// Query selects what a status report carries.
type Query struct {
	// JobID narrows the report to one job; empty reports the queue.
	JobID string `json:"job_id,omitempty"`
	// Limit caps the jobs reported; zero takes the daemon's default.
	Limit int `json:"limit,omitempty"`
	// Await marks JobID as awaited: while such reads keep arriving, the daemon
	// deletes that job ahead of every job nobody awaits, at the governed pace.
	Await bool `json:"await,omitempty"`
}

// Governor is the deletion throttle as an observer reads it.
type Governor struct {
	// State is idle, sampling, clear, throttled, or unavailable. Deletion is
	// capped at its floor rate while throttled or unavailable.
	State string `json:"state"`
	// CPUPercent is fseventsd's last sampled CPU use, 100 being one core.
	CPUPercent float64 `json:"cpu_percent"`
	// Detail names what an unavailable sampler failed on, or says that slices
	// stalled past twice their time budget, whether or not CPU throttles too.
	Detail string `json:"detail,omitempty"`
}

// Report is the daemon's bounded view of its queue: unfinished jobs first in
// queue order, then the most recently finished, with no total file count and
// no estimate of time remaining.
type Report struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Paused reports that physical deletion is stopped queue-wide, by a pause
	// or a maintenance hold.
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

// HoldRequest asks for the operator's maintenance hold on the queue.
type HoldRequest struct {
	// ID names the holder; only a Release naming it again lifts the hold.
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Validate rejects a request that names no holder or no reason.
func (r HoldRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.ID) == "":
		return errors.New("the hold names no holder")
	case strings.TrimSpace(r.Reason) == "":
		return errors.New("the hold names no reason")
	}
	return nil
}

// Hold is an operator's maintenance hold on the queue. While it stands the
// queue rests as a pause rests it and Resume is refused; it never expires, and
// only a Release naming its ID lifts it.
type Hold struct {
	ID     string    `json:"id"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// Validate rejects a hold with no holder, no reason, or no start.
func (h Hold) Validate() error {
	if h.Since.IsZero() {
		return errors.New("the hold records no start")
	}
	return HoldRequest{ID: h.ID, Reason: h.Reason}.Validate()
}

// HoldState is the queue's stop as an operator sets it: the ordinary pause and
// the maintenance hold apart, Hold nil when none stands.
type HoldState struct {
	Paused bool  `json:"paused"`
	Hold   *Hold `json:"hold"`
}

// HoldReceipt is the hold standing once a Hold request returns. Already
// reports that the same holder held the queue before, so nothing changed.
type HoldReceipt struct {
	Hold    Hold `json:"hold"`
	Already bool `json:"already,omitempty"`
}

// HeldError is a request the maintenance hold refused: a Resume, a Hold by
// another holder, or a Release naming anyone but the holder. It names the
// hold that stands.
type HeldError struct {
	Hold Hold
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("cleanup: %s holds the queue for maintenance since %s: %s; only ccx vcs cleanup hold release --id %q lifts it", e.Hold.ID, e.Hold.Since.Local().Format(time.RFC3339), e.Hold.Reason, e.Hold.ID)
}

// Service is the daemon as a caller uses it, in process or over its socket.
type Service interface {
	// Remove relocates and unregisters a worktree and returns once the removal
	// is logically complete. A preflight refusal is a *RefusedError or an
	// *ActiveError with nothing journaled; a removal that stopped part-way is a
	// *BlockedError. While the queue is paused it journals the removal at
	// PhasePending after the preflight's checks that look for no live process,
	// and returns with nothing moved; the daemon then retires the registration
	// in place, and Resume moves and deletes the tree.
	Remove(ctx context.Context, r Request) (Receipt, error)
	// Defer journals a removal that waits for inactivity and returns without
	// moving anything.
	Defer(ctx context.Context, r DeferRequest) (Receipt, error)
	// Status reports the queue, or one job.
	Status(ctx context.Context, q Query) (Report, error)
	// Wait returns once the job is done, or with a *BlockedError once it
	// blocks.
	Wait(ctx context.Context, jobID string) (Job, error)
	// Pause stops the queue from moving or deleting any tree, Remove journals a
	// new removal as pending, and Defer and Adopt return ErrPaused until
	// Resume, across restarts. Untouched removals are still retired in place.
	Pause(ctx context.Context) error
	// Resume lets the queue run again, rested and pending jobs and new
	// removals alike. While a maintenance hold stands it is a *HeldError and
	// changes nothing.
	Resume(ctx context.Context) error
	// Hold takes the operator's maintenance hold and returns once the deletion
	// in flight has parked: the queue rests as Pause rests it, across
	// restarts, until Release names the same ID. A repeat by the holder
	// changes nothing; another holder's is a *HeldError.
	Hold(ctx context.Context, r HoldRequest) (HoldReceipt, error)
	// Release lifts the hold id names and nothing else: an ordinary pause
	// stays. A hold under another ID is a *HeldError and no hold at all is
	// ErrNotHeld, either leaving the queue as it was.
	Release(ctx context.Context, id string) error
	// HoldState reads the ordinary pause and the maintenance hold apart, never
	// waiting behind the worker once it has loaded the journal.
	HoldState(ctx context.Context) (HoldState, error)
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

// Peer is the process serving the daemon socket, as the kernel reports it for
// one connection.
type Peer struct {
	PID int
	UID int
}

// Control is a Service reached over the daemon's socket, with the verbs a
// client needs to replace the daemon behind it.
type Control interface {
	Service
	// Hello identifies the serving daemon.
	Hello(ctx context.Context) (Info, error)
	// Greet is Hello on one connection whose kernel peer observe receives
	// before the hello is sent, so what observe records belongs to the
	// process whose reply follows.
	Greet(ctx context.Context, observe func(Peer)) (Info, error)
	// Shutdown stops the daemon after its current step and journal flush,
	// returning once it stops serving. It sends only on a connection whose
	// Peer verify accepts and returns a refusal with nothing sent.
	Shutdown(ctx context.Context, verify func(Peer) error) error
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
	msg := fmt.Sprintf("cleanup job %s is blocked at %s (%s): %s", e.Job.ID, e.Job.Phase, b.Reason, b.Detail)
	if b.RetryAt != nil {
		msg += "; the daemon retries it at " + b.RetryAt.Local().Format(time.TimeOnly)
	}
	return msg
}
