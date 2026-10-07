package ghapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrNotFound reports a 404, or a GraphQL error typed NOT_FOUND — the
	// answer callers branch on instead of matching an error's message text.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized reports a 401 that survived a token re-resolve: the
	// credentials gh holds do not authenticate here.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNoToken reports that no token could be resolved at all — no env var
	// set and no usable gh.
	ErrNoToken = errors.New("no github token")
	// ErrPaginationCycle reports a Link header chain that names a page the walk
	// already fetched, so no number of further requests would end it.
	ErrPaginationCycle = errors.New("link chain revisits a page already fetched")
)

// StatusError is a request that reached GitHub and came back non-2xx. It
// unwraps to ErrNotFound on 404 and ErrUnauthorized on 401. Limited marks a
// rate-limit refusal, primary or secondary, and RetryAfter the wait it named.
type StatusError struct {
	Method     string
	URL        string
	Status     int
	Message    string
	Limited    bool
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	return fmt.Sprintf("ghapi: %s %s: %d %s", e.Method, e.URL, e.Status, message)
}

func (e *StatusError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return ErrUnauthorized
	}
	return nil
}

// GraphQLMessage is one entry of a GraphQL response's errors array.
type GraphQLMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// GraphQLError is a 200 response carrying GraphQL errors, unwrapping to
// ErrNotFound on a NOT_FOUND entry. Exhausted marks a spent quota of Limit.
type GraphQLError struct {
	Messages   []GraphQLMessage
	Exhausted  bool
	RetryAfter time.Duration
	Limit      int
}

func (e *GraphQLError) Error() string {
	texts := make([]string, 0, len(e.Messages))
	for _, m := range e.Messages {
		texts = append(texts, m.Message)
	}
	return "ghapi: graphql: " + strings.Join(texts, "; ")
}

func (e *GraphQLError) Unwrap() error {
	for _, m := range e.Messages {
		if m.Type == "NOT_FOUND" {
			return ErrNotFound
		}
	}
	return nil
}

// statusError reads GitHub's error envelope off payload; a body that is not
// that envelope (a proxy's 502 page) leaves Message empty for Error to fill. A
// secondary limit answers 403 with no quota headers, so its message marks it.
func statusError(method, target string, status int, header http.Header, payload []byte, now time.Time) *StatusError {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &body)
	wait, named := limitWait(status, header, now)
	limited := named || status == http.StatusTooManyRequests ||
		status == http.StatusForbidden && strings.Contains(strings.ToLower(body.Message), "rate limit")
	return &StatusError{Method: method, URL: target, Status: status, Message: body.Message, Limited: limited, RetryAfter: wait}
}
