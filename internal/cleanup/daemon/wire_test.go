package daemon

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestWireErrorsSurviveStrictDecoding(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind string
		is   error
	}{
		{"paused", cleanup.ErrPaused, kindPaused, cleanup.ErrPaused},
		{"paused under a wrapper", fmt.Errorf("cleanup daemon: remove: %w", cleanup.ErrPaused), kindPaused, cleanup.ErrPaused},
		{"unknown job", cleanup.ErrUnknownJob, kindUnknownJob, cleanup.ErrUnknownJob},
		{"untyped", errors.New("could not verify activity: pid 9: operation not permitted"), kindInternal, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sent := wireErrorOf(tt.err)
			if sent.Kind != tt.kind || sent.Message != tt.err.Error() {
				t.Fatalf("wireErrorOf() = kind %q, message %q; want %q, %q", sent.Kind, sent.Message, tt.kind, tt.err.Error())
			}
			data, err := durable.Marshal(response{Error: sent})
			if err != nil {
				t.Fatalf("Marshal() = %v", err)
			}
			reply, err := durable.Unmarshal[response](data)
			if err != nil {
				t.Fatalf("Unmarshal(%s) = %v", data, err)
			}
			rebuilt := reply.Error.rebuild()
			if tt.is == nil {
				if rebuilt.Error() != tt.err.Error() || errors.Is(rebuilt, cleanup.ErrPaused) || errors.Is(rebuilt, cleanup.ErrUnknownJob) {
					t.Errorf("rebuilt = %v, want the message alone, matching no sentinel", rebuilt)
				}
				return
			}
			if !errors.Is(rebuilt, tt.is) {
				t.Errorf("rebuilt = %v, want it to match %v", rebuilt, tt.is)
			}
		})
	}
}

func TestStatusAwaitNamesItsJob(t *testing.T) {
	tests := []struct {
		name    string
		query   cleanup.Query
		wantErr string
	}{
		{"an awaiting read of one job", cleanup.Query{JobID: "0000000000000000-000000", Await: true}, ""},
		{"an awaiting read of the queue", cleanup.Query{Await: true}, "status awaits no job"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := request{Version: "v1.2.3", Op: opStatus, Query: &tt.query}.Validate()
			if got := fmt.Sprint(err); (err == nil) != (tt.wantErr == "") || (err != nil && got != tt.wantErr) {
				t.Errorf("Validate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
