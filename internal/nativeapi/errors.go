package nativeapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var (
	// ErrUnauthorized identifies a rejected authentication credential.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNoToken identifies missing GitHub authentication.
	ErrNoToken = errors.New("no GitHub token")
	// ErrNoQueueKey is a Trunk queue read without TRUNK_MERGE_API_KEY.
	ErrNoQueueKey = errors.New("trunk queue status requires TRUNK_MERGE_API_KEY with a scoped v2 API key")
)

// StatusError retains the refused HTTP operation and the server diagnostic.
type StatusError struct {
	Method  string
	URL     string
	Status  int
	Message string
}

// Error includes the refused operation and the server diagnostic.
func (e *StatusError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	return fmt.Sprintf("github-stack: %s %s: %d %s", e.Method, e.URL, e.Status, message)
}

// Unwrap preserves authentication checks through errors.Is.
func (e *StatusError) Unwrap() error {
	if e.Status == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	return nil
}

// ResultError reports a GraphQL failure returned inside a successful HTTP response.
type ResultError struct {
	Route   string
	Message string
}

// Error names the GraphQL operation and its failure diagnostic.
func (e *ResultError) Error() string {
	return fmt.Sprintf("github-stack: %s: %s", e.Route, e.Message)
}

// BranchSubmitError identifies the branch whose submission could not finish.
type BranchSubmitError struct {
	Head    string
	Message string
}

// SubmitError retains both actual PR identities and unfinished branch submissions.
type SubmitError struct {
	Cause     error
	Submitted []SubmittedPR
	Failed    []BranchSubmitError
}

// Unwrap keeps the request failure reachable through a partial submission.
func (e *SubmitError) Unwrap() error { return e.Cause }

// Error names unfinished branches and summarizes partial submission results.
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
		var message string
		for _, named := range []string{body.Message, body.Error, body.Detail} {
			if named != "" {
				message = named
				break
			}
		}
		if len(body.Errors) > 0 && string(body.Errors) != "null" {
			if message != "" {
				message += ": "
			}
			message += string(body.Errors)
		}
		if message != "" {
			return statusQuote([]byte(message))
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
