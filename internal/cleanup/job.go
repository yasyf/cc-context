package cleanup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const (
	// Schema is the version of the durable job record.
	Schema = 1
	// RecoveryRefPrefix is the ref namespace pinning each removed worktree's
	// committed HEAD.
	RecoveryRefPrefix = "refs/ccx/cleanup/"
	// MaxJobErrors bounds the error history one record carries.
	MaxJobErrors = 8
)

// Phase is how far a removal has durably progressed. A record is journaled at
// each phase before the step that leaves it, so the phase names the last state
// the filesystem is known to have reached and the only steps that may have run
// since.
type Phase string

const (
	// PhaseQueued is a deferred removal nobody has examined yet. Nothing is
	// mutated.
	PhaseQueued Phase = "queued"
	// PhaseWaiting is a deferred removal whose tree a live process still holds.
	// Nothing is mutated.
	PhaseWaiting Phase = "waiting"
	// PhasePrepared is journaled before the first mutation: from here watchers
	// may be retired, the recovery ref written, and git's move attempted.
	PhasePrepared Phase = "prepared"
	// PhaseMoved means git registers the tree at Registered and the original
	// path is free.
	PhaseMoved Phase = "moved"
	// PhaseDetached means the tree sits at Payload and git's registration names
	// a path that no longer exists.
	PhaseDetached Phase = "detached"
	// PhaseUnregistered means the registration is gone: the removal is
	// logically complete and only the payload's deletion remains.
	PhaseUnregistered Phase = "unregistered"
	// PhaseDeleting means physical deletion of the payload has begun.
	PhaseDeleting Phase = "deleting"
	// PhaseDone means the payload is gone.
	PhaseDone Phase = "done"
)

var phaseRank = map[Phase]int{
	PhaseQueued:       0,
	PhaseWaiting:      1,
	PhasePrepared:     2,
	PhaseMoved:        3,
	PhaseDetached:     4,
	PhaseUnregistered: 5,
	PhaseDeleting:     6,
	PhaseDone:         7,
}

// Logical reports whether the phase still belongs to the logical half: the
// tree is not yet both relocated and unregistered.
func (p Phase) Logical() bool { return phaseRank[p] < phaseRank[PhaseUnregistered] }

// Physical reports whether only the payload's deletion remains.
func (p Phase) Physical() bool { return p == PhaseUnregistered || p == PhaseDeleting }

// State is a job's phase as an observer reads it, with a blockage layered over
// the phase the job stopped in.
type State string

// StateBlocked is a job that needs an operator before it moves again.
const StateBlocked State = "blocked"

// FileID is a file's identity on one machine: renames preserve it, and a
// replacement at the same path never shares it.
type FileID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// Zero reports whether the identity was never captured.
func (id FileID) Zero() bool { return id == FileID{} }

// IDOf returns the identity info's stat carries.
func IDOf(info os.FileInfo) FileID {
	st := info.Sys().(*syscall.Stat_t)
	return FileID{Dev: uint64(st.Dev), Ino: uint64(st.Ino)} //nolint:gosec // Dev is int32 on darwin and uint64 on linux; the widening is an identity key, never arithmetic
}

// LstatID returns the identity of whatever path names, without following a
// final symlink.
func LstatID(path string) (FileID, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileID{}, nil, err
	}
	return IDOf(info), info, nil
}

// Links is the exact content of the two files that tie a linked worktree to
// its repository, as they read before the removal touched anything.
type Links struct {
	// DotGit is the worktree's .git file.
	DotGit string `json:"dot_git"`
	// AdminGitdir is the gitdir file in the worktree's admin directory.
	AdminGitdir string `json:"admin_gitdir"`
}

// Blockage is why a job stopped and what an operator needs to know to move it.
type Blockage struct {
	// Reason is a short stable category: activity, identity, dirty, watchers,
	// quarantine, git, timeout, reconcile, delete, or journal.
	Reason string    `json:"reason"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// JobError is one entry of a job's bounded failure history.
type JobError struct {
	At      time.Time `json:"at"`
	Phase   Phase     `json:"phase"`
	Message string    `json:"message"`
}

// Job is the durable record of one removal. Every field that identifies what
// the removal may touch is captured before the first mutation and never
// rewritten, so a restart acts only on the tree the record names.
type Job struct {
	Schema int    `json:"schema"`
	ID     string `json:"id"`
	// Seq orders jobs first-in first-out.
	Seq   uint64 `json:"seq"`
	Phase Phase  `json:"phase"`
	// Deferred marks a removal its owner recorded while the tree was still
	// occupied: it waits for inactivity instead of refusing.
	Deferred bool `json:"deferred,omitempty"`
	// Adopted marks a tree the retired janitor had already unregistered and
	// parked at Source in its quarantine. It has no admin directory and no
	// links to capture: the job takes the tree from Source straight to Payload.
	Adopted bool   `json:"adopted,omitempty"`
	Source  string `json:"source,omitempty"`
	Owner   string `json:"owner,omitempty"`
	// Repo is the canonical git common directory.
	Repo string `json:"repo"`
	// AdminDir is the worktree's admin directory, <Repo>/worktrees/<id>; empty
	// for an adopted tree.
	AdminDir string `json:"admin_dir,omitempty"`
	Admin    FileID `json:"admin"`
	// Original is the canonical path the worktree was registered at.
	Original   string `json:"original"`
	Registered string `json:"registered"`
	Payload    string `json:"payload"`
	Tree       FileID `json:"tree"`
	// Head is the committed HEAD, empty for an unborn branch.
	Head   string `json:"head,omitempty"`
	Branch string `json:"branch,omitempty"`
	// RecoveryRef pins Head in Repo; empty exactly when Head is. An adopted
	// tree keeps the legacy ref the retired janitor wrote.
	RecoveryRef string `json:"recovery_ref,omitempty"`
	Force       bool   `json:"force,omitempty"`
	// Git is the absolute git binary every step of this job runs.
	Git     string     `json:"git"`
	Links   Links      `json:"links"`
	Blocked *Blockage  `json:"blocked,omitempty"`
	Errors  []JobError `json:"errors,omitempty"`
	// Removed counts the payload entries deleted so far.
	Removed uint64    `json:"removed,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

var (
	jobIDPattern = regexp.MustCompile(`^[0-9a-f]{16}-[0-9a-f]{6}$`)
	oidPattern   = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// MintID returns a fresh job id: the creation instant in fixed-width hex, so
// job folders list in creation order, and a random suffix.
func MintID(now time.Time) string {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		panic(fmt.Sprintf("cleanup: read random bytes: %v", err))
	}
	return fmt.Sprintf("%016x-%s", uint64(now.UnixNano()), hex.EncodeToString(suffix[:])) //nolint:gosec // a wall clock after 1970 is non-negative
}

// RecoveryRefFor is the ref that pins the committed HEAD of job id.
func RecoveryRefFor(id string) string { return RecoveryRefPrefix + id }

// State is the job's phase as an observer reads it.
func (j Job) State() State {
	if j.Blocked != nil {
		return StateBlocked
	}
	return State(j.Phase)
}

// Note appends message to the bounded error history, dropping the oldest entry
// past MaxJobErrors.
func (j *Job) Note(at time.Time, message string) {
	j.Errors = append(j.Errors, JobError{At: at, Phase: j.Phase, Message: message})
	if over := len(j.Errors) - MaxJobErrors; over > 0 {
		j.Errors = j.Errors[over:]
	}
}

// Block stops the job for an operator, recording why in both the blockage and
// the error history.
func (j *Job) Block(at time.Time, reason, detail string) {
	j.Blocked = &Blockage{Reason: reason, Detail: detail, At: at}
	j.Note(at, reason+": "+detail)
	j.Updated = at
}

// Validate rejects any record whose fields could name something other than one
// tree and its private job folder.
func (j Job) Validate() error {
	if j.Schema != Schema {
		return fmt.Errorf("schema %d, want %d", j.Schema, Schema)
	}
	if !jobIDPattern.MatchString(j.ID) {
		return fmt.Errorf("id %q is not a job id", j.ID)
	}
	if j.Seq == 0 {
		return errors.New("seq is zero")
	}
	if _, ok := phaseRank[j.Phase]; !ok {
		return fmt.Errorf("unknown phase %q", j.Phase)
	}
	if !j.Deferred && (j.Phase == PhaseQueued || j.Phase == PhaseWaiting) {
		return fmt.Errorf("phase %q belongs to a deferred job", j.Phase)
	}
	for name, path := range map[string]string{
		"repo": j.Repo, "original": j.Original, "registered": j.Registered, "payload": j.Payload, "git": j.Git,
	} {
		if !cleanAbs(path) {
			return fmt.Errorf("%s %q is not a clean absolute path", name, path)
		}
	}
	jobDir := filepath.Dir(j.Registered)
	if filepath.Base(jobDir) != j.ID || filepath.Base(j.Registered) != registeredName {
		return fmt.Errorf("registered %q is not job %s's private path", j.Registered, j.ID)
	}
	if j.Payload != filepath.Join(jobDir, PayloadName) {
		return fmt.Errorf("payload %q is not job %s's private path", j.Payload, j.ID)
	}
	if j.Tree.Zero() {
		return errors.New("tree identity was never captured")
	}
	if j.Head != "" && !oidPattern.MatchString(j.Head) {
		return fmt.Errorf("head %q is not an object id", j.Head)
	}
	identity := j.validateLinked
	if j.Adopted {
		identity = j.validateAdopted
	}
	if err := identity(); err != nil {
		return err
	}
	if j.Blocked != nil && (j.Blocked.Reason == "" || j.Blocked.Detail == "") {
		return errors.New("blockage carries no reason or detail")
	}
	if len(j.Errors) > MaxJobErrors {
		return fmt.Errorf("%d errors, at most %d", len(j.Errors), MaxJobErrors)
	}
	if j.Created.IsZero() || j.Updated.IsZero() {
		return errors.New("created or updated is unset")
	}
	return nil
}

func (j Job) validateLinked() error {
	if j.Source != "" {
		return fmt.Errorf("source %q set on a job that adopted nothing", j.Source)
	}
	if !cleanAbs(j.AdminDir) {
		return fmt.Errorf("admin_dir %q is not a clean absolute path", j.AdminDir)
	}
	if filepath.Dir(filepath.Dir(j.AdminDir)) != j.Repo || filepath.Base(filepath.Dir(j.AdminDir)) != "worktrees" {
		return fmt.Errorf("admin_dir %q is not a worktree admin directory of %q", j.AdminDir, j.Repo)
	}
	if j.Admin.Zero() {
		return errors.New("tree or admin identity was never captured")
	}
	switch {
	case j.Head == "" && j.RecoveryRef != "":
		return fmt.Errorf("recovery_ref %q set with no head to pin", j.RecoveryRef)
	case j.Head != "" && j.RecoveryRef != RecoveryRefFor(j.ID):
		return fmt.Errorf("recovery_ref %q, want %q", j.RecoveryRef, RecoveryRefFor(j.ID))
	}
	if j.Links.DotGit == "" || j.Links.AdminGitdir == "" {
		return errors.New("link contents were never captured")
	}
	return nil
}

func (j Job) validateAdopted() error {
	if j.Deferred {
		return errors.New("an adopted job cannot be deferred")
	}
	if j.Phase == PhaseMoved || j.Phase == PhaseDetached {
		return fmt.Errorf("phase %q belongs to a registered worktree, not an adopted tree", j.Phase)
	}
	if !cleanAbs(j.Source) {
		return fmt.Errorf("source %q is not a clean absolute path", j.Source)
	}
	if j.AdminDir != "" || !j.Admin.Zero() || j.Links != (Links{}) {
		return errors.New("an adopted job carries admin or link state it never had")
	}
	if j.Head == "" {
		return errors.New("an adopted job carries no head")
	}
	return LegacyBinding(j.Source, j.RecoveryRef)
}

func cleanAbs(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}
