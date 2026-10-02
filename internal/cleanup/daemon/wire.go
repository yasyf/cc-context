package daemon

import (
	"errors"
	"fmt"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	opHello    = "hello"
	opRemove   = "remove"
	opDefer    = "defer"
	opAdopt    = "adopt"
	opStatus   = "status"
	opWait     = "wait"
	opPause    = "pause"
	opResume   = "resume"
	opRetry    = "retry"
	opShutdown = "shutdown"

	kindRefused      = "refused"
	kindActive       = "active"
	kindBlocked      = "blocked"
	kindUnknownJob   = "unknown_job"
	kindPaused       = "paused"
	kindIncompatible = "incompatible"
	kindInternal     = "internal"

	maxRequestBytes = 1 << 20
	maxReplyBytes   = 64 << 20
)

// ErrIncompatible reports a daemon that speaks another protocol version and
// executed nothing.
var ErrIncompatible = errors.New("cleanup daemon: incompatible protocol")

type head struct {
	Protocol int    `json:"protocol"`
	Op       string `json:"op"`
}

type request struct {
	Protocol int                   `json:"protocol"`
	Version  string                `json:"version"`
	Op       string                `json:"op"`
	Remove   *cleanup.Request      `json:"remove,omitempty"`
	Defer    *cleanup.DeferRequest `json:"defer,omitempty"`
	Adopt    *cleanup.AdoptRequest `json:"adopt,omitempty"`
	Query    *cleanup.Query        `json:"query,omitempty"`
	JobID    string                `json:"job_id,omitempty"`
}

func (r request) Validate() error {
	if r.Version == "" {
		return errors.New("the request names no client version")
	}
	switch r.Op {
	case opHello, opShutdown, opPause, opResume:
		if r.Remove != nil || r.Defer != nil || r.Adopt != nil || r.Query != nil || r.JobID != "" {
			return fmt.Errorf("%s carries nothing beyond its op", r.Op)
		}
		return nil
	case opRemove:
		if r.Remove == nil {
			return errors.New("remove carries no request")
		}
		return r.Remove.Validate()
	case opDefer:
		if r.Defer == nil {
			return errors.New("defer carries no request")
		}
		return r.Defer.Validate()
	case opAdopt:
		if r.Adopt == nil {
			return errors.New("adopt carries no request")
		}
		return r.Adopt.Validate()
	case opStatus:
		if r.Query == nil {
			return errors.New("status carries no query")
		}
		return nil
	case opWait, opRetry:
		if r.JobID == "" {
			return fmt.Errorf("%s names no job", r.Op)
		}
		return nil
	}
	return fmt.Errorf("unknown op %q", r.Op)
}

type response struct {
	Error   *wireError       `json:"error,omitempty"`
	Info    *cleanup.Info    `json:"info,omitempty"`
	Receipt *cleanup.Receipt `json:"receipt,omitempty"`
	Report  *cleanup.Report  `json:"report,omitempty"`
	Job     *cleanup.Job     `json:"job,omitempty"`
}

func (r response) Validate() error {
	if r.Error == nil {
		return nil
	}
	return r.Error.validate()
}

type wireError struct {
	Kind    string                `json:"kind"`
	Message string                `json:"message"`
	Refused *cleanup.RefusedError `json:"refused,omitempty"`
	Active  *cleanup.ActiveError  `json:"active,omitempty"`
	Blocked *cleanup.BlockedError `json:"blocked,omitempty"`
}

func (w *wireError) validate() error {
	switch w.Kind {
	case kindUnknownJob, kindPaused, kindIncompatible, kindInternal:
		return nil
	case kindRefused:
		if w.Refused == nil {
			return errors.New("refused error carries no refusal")
		}
		return nil
	case kindActive:
		if w.Active == nil {
			return errors.New("active error carries no holders")
		}
		return nil
	case kindBlocked:
		if w.Blocked == nil || w.Blocked.Job.Blocked == nil {
			return errors.New("blocked error carries no blocked job")
		}
		return nil
	}
	return fmt.Errorf("unknown error kind %q", w.Kind)
}

func (w *wireError) rebuild() error {
	switch w.Kind {
	case kindRefused:
		return w.Refused
	case kindActive:
		return w.Active
	case kindBlocked:
		return w.Blocked
	case kindUnknownJob:
		return cleanup.ErrUnknownJob
	case kindPaused:
		return cleanup.ErrPaused
	case kindIncompatible:
		return fmt.Errorf("%w: %s", ErrIncompatible, w.Message)
	}
	return errors.New(w.Message)
}

func wireErrorOf(err error) *wireError {
	var (
		refused *cleanup.RefusedError
		active  *cleanup.ActiveError
		blocked *cleanup.BlockedError
	)
	switch {
	case errors.As(err, &refused):
		return &wireError{Kind: kindRefused, Message: err.Error(), Refused: refused}
	case errors.As(err, &active):
		return &wireError{Kind: kindActive, Message: err.Error(), Active: active}
	case errors.As(err, &blocked):
		return &wireError{Kind: kindBlocked, Message: err.Error(), Blocked: blocked}
	case errors.Is(err, cleanup.ErrUnknownJob):
		return &wireError{Kind: kindUnknownJob, Message: err.Error()}
	case errors.Is(err, cleanup.ErrPaused):
		return &wireError{Kind: kindPaused, Message: err.Error()}
	}
	return &wireError{Kind: kindInternal, Message: err.Error()}
}
