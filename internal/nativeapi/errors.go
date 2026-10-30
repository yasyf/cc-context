package nativeapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrNoToken      = errors.New("no GitHub token")
)

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
	return fmt.Sprintf("github-stack: %s %s: %d %s", e.Method, e.URL, e.Status, message)
}

func (e *StatusError) Unwrap() error {
	if e.Status == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	return nil
}

type ResultError struct {
	Route   string
	Message string
}

func (e *ResultError) Error() string {
	return fmt.Sprintf("github-stack: %s: %s", e.Route, e.Message)
}

type BranchSubmitError struct {
	Head    string
	Message string
}

type SubmitError struct {
	Cause     error
	Submitted []SubmittedPR
	Failed    []BranchSubmitError
}

func (e *SubmitError) Unwrap() error { return e.Cause }

func (e *SubmitError) Error() string {
	texts := make([]string, 0, len(e.Failed))
	for _, f := range e.Failed {
		texts = append(texts, f.Head+": "+f.Message)
	}
	return fmt.Sprintf("github-stack: submit: %d of %d branches failed: %s",
		len(e.Failed), len(e.Failed)+len(e.Submitted), strings.Join(texts, "; "))
}

const statusBodyCap = 512

func statusError(method, target string, status int, payload []byte) *StatusError {
	return &StatusError{Method: method, URL: target, Status: status, Message: statusMessage(payload)}
}

func statusMessage(payload []byte) string {
	var body struct {
		Message string          `json:"message"`
		Error   string          `json:"error"`
		Detail  string          `json:"detail"`
		Errors  json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(payload, &body); err == nil {
		for _, named := range []string{body.Message, body.Error, body.Detail} {
			if named != "" {
				return named
			}
		}
		if len(body.Errors) > 0 && string(body.Errors) != "null" {
			return statusQuote(body.Errors)
		}
	}
	return statusQuote(payload)
}

func statusQuote(payload []byte) string {
	quoted := strings.Join(strings.Fields(string(payload)), " ")
	if len(quoted) > statusBodyCap {
		return quoted[:statusBodyCap] + "…"
	}
	return quoted
}
