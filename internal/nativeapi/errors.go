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
	// ErrNoTrunkKey identifies a Trunk API key no source yields without a sign-in.
	ErrNoTrunkKey = errors.New("no Trunk key: set TRUNK_MERGE_API_KEY, add the trunk-merge-api-key Keychain item, or sign in to the [trunk] aws_profile")
)

type lineBaseError struct {
	number       int
	base, remedy string
}

func (e lineBaseError) Error() string {
	return fmt.Sprintf("PR #%d parent branch %s has no open pull request, so it cannot join a native stack; move it onto trunk with %s", e.number, e.base, e.remedy)
}

type loneForkError struct {
	number, parent, stack int
	base                  string
}

func (e loneForkError) Error() string {
	return fmt.Sprintf("PR #%d sits on #%d %s outside native GitHub stack #%d, where another open pull request already follows #%d; GitHub has no stack of one pull request, so #%d opens a stack of its own once it has a child", e.number, e.parent, e.base, e.stack, e.parent, e.number)
}

// StatusError retains the refused HTTP operation and the server diagnostic.
type StatusError struct {
	Method  string
	URL     string
	Status  int
	Message string
	Payload []byte
	Header  http.Header
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
	return &StatusError{Method: method, URL: target, Status: status, Message: statusMessage(payload), Payload: payload}
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
