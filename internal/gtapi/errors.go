package gtapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var (
	// ErrUnauthorized reports a 401: the token gt holds does not authenticate
	// here.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNoToken reports that ~/.config/graphite/auth could not be read or
	// carries no token.
	ErrNoToken = errors.New("no graphite token")
)

// StatusError is a request that reached Graphite and came back non-2xx. It
// unwraps to ErrUnauthorized on 401.
type StatusError struct {
	Method  string
	URL     string
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	return fmt.Sprintf("gtapi: %s %s: %d %s", e.Method, e.URL, e.Status, message)
}

func (e *StatusError) Unwrap() error {
	if e.Status == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	return nil
}

// ResultError is a 200 response whose result envelope carried the route's
// error variant instead of its payload.
type ResultError struct {
	Route   string
	Message string
}

func (e *ResultError) Error() string {
	return fmt.Sprintf("gtapi: %s: %s", e.Route, e.Message)
}

// BranchSubmitError is one branch a submit refused.
type BranchSubmitError struct {
	Head    string
	Message string
}

// SubmitError reports a submit that failed for some branches; Submitted holds
// the branches that landed anyway, so callers can report both sides exactly.
type SubmitError struct {
	Submitted []SubmittedPR
	Failed    []BranchSubmitError
}

func (e *SubmitError) Error() string {
	texts := make([]string, 0, len(e.Failed))
	for _, f := range e.Failed {
		texts = append(texts, f.Head+": "+f.Message)
	}
	return fmt.Sprintf("gtapi: submit: %d of %d branches failed: %s",
		len(e.Failed), len(e.Failed)+len(e.Submitted), strings.Join(texts, "; "))
}

func statusError(method, target string, status int, payload []byte) *StatusError {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &body)
	return &StatusError{Method: method, URL: target, Status: status, Message: body.Message}
}
