package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// hostCallValve caps a single host call's string return, so one careless call
// (a full-repo grep, an un-scoped read) cannot flood the sandbox. It bounds
// each call, not the run: the final budget cap in Run is separate.
const hostCallValve = 4 << 20

// hostAbandonTimeout bounds how long pump waits for in-flight host calls after
// the run ends: a call that ignores cancellation (a reflected third-party MCP
// tool, say) is abandoned to its goroutine instead of wedging Run.
const hostAbandonTimeout = 5 * time.Second

// initFrame is the first line the host writes: everything the driver needs to
// compile, typecheck, and run one script.
type initFrame struct {
	Script    string         `json:"script"`
	Stubs     string         `json:"stubs"`
	Functions []string       `json:"functions"`
	Limits    map[string]any `json:"limits"`
}

// resultFrame answers one call frame; ok false raises Error into the sandbox.
type resultFrame struct {
	ID    int64           `json:"id"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// driverFrame is any line the driver writes: t is "ready", "call", or "done",
// with the remaining fields populated per type.
type driverFrame struct {
	T      string                     `json:"t"`
	ID     int64                      `json:"id"`
	Fn     string                     `json:"fn"`
	Args   []json.RawMessage          `json:"args"`
	Kwargs map[string]json.RawMessage `json:"kwargs"`
	OK     bool                       `json:"ok"`
	Phase  string                     `json:"phase"`
	Value  json.RawMessage            `json:"value"`
	Error  string                     `json:"error"`
	Stdout string                     `json:"stdout"`
}

// pump drives one run over the driver's stream: it writes the init frame,
// dispatches every call frame to its host function on its own goroutine (the
// read loop never blocks on host work), and returns the done frame. stderr
// supplies the driver's stderr tail for crash reports.
func pump(ctx context.Context, w io.Writer, r io.Reader, stderr func() string, init initFrame, funcs map[string]HostFunc) (*driverFrame, error) {
	ctx, cancel := context.WithCancel(ctx)
	g, gctx := errgroup.WithContext(ctx)
	defer func() {
		cancel()
		waited := make(chan struct{})
		go func() { _ = g.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(hostAbandonTimeout):
		}
	}()

	var wmu sync.Mutex
	enc := json.NewEncoder(w)
	write := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return enc.Encode(v)
	}
	// A stuck init write (multi-MB script, driver still inside uv resolution)
	// stays cancellable: w is the child's os.Pipe-backed stdin, so Run's
	// AfterFunc Close unblocks the Write via the poller, and the context kill
	// breaks the pipe besides.
	if err := write(init); err != nil {
		return nil, fmt.Errorf("codeexec: write init frame: %w", err)
	}

	dec := json.NewDecoder(r)
	seen := make(map[int64]bool)
	for {
		var f driverFrame
		if err := dec.Decode(&f); err != nil {
			return nil, crashError(err, stderr())
		}
		switch f.T {
		case "ready":
		case "call":
			if seen[f.ID] {
				return nil, fmt.Errorf("codeexec: driver reused call id %d", f.ID)
			}
			seen[f.ID] = true
			g.Go(func() error { return write(dispatch(gctx, funcs, f)) })
		case "done":
			return &f, nil
		default:
			return nil, fmt.Errorf("codeexec: unknown driver frame %q", f.T)
		}
	}
}

// dispatch runs one host call and shapes its outcome as the result frame: an
// error and an over-valve return both raise into the sandbox.
func dispatch(ctx context.Context, funcs map[string]HostFunc, f driverFrame) resultFrame {
	fn, ok := funcs[f.Fn]
	if !ok {
		return resultFrame{ID: f.ID, Error: fmt.Sprintf("codeexec: no host function %q", f.Fn)}
	}
	call, err := decodeCall(f)
	if err != nil {
		return resultFrame{ID: f.ID, Error: fmt.Sprintf("codeexec: decode %s arguments: %v", f.Fn, err)}
	}
	val, err := fn(ctx, call)
	if err != nil {
		return resultFrame{ID: f.ID, Error: err.Error()}
	}
	// Reject an oversized raw string/[]byte before encoding it, then re-check
	// the encoded size so structured returns (map/slice) hit the valve too —
	// HostFunc returns any, so stringSize alone would let them through.
	if size := stringSize(val); size > hostCallValve {
		return valveExceeded(f.ID, size)
	}
	enc, err := encodeValue(val)
	if err != nil {
		return resultFrame{ID: f.ID, Error: fmt.Sprintf("codeexec: encode %s result: %v", f.Fn, err)}
	}
	if len(enc) > hostCallValve {
		return valveExceeded(f.ID, len(enc))
	}
	return resultFrame{ID: f.ID, OK: true, Value: enc}
}

func valveExceeded(id int64, size int) resultFrame {
	return resultFrame{ID: id, Error: fmt.Sprintf(
		"codeexec valve: host call returned %d bytes (per-call limit %d); narrow the call with a tighter scope, section, or glob instead of reading it whole",
		size, hostCallValve)}
}

func crashError(err error, tail string) error {
	if tail != "" {
		return fmt.Errorf("codeexec: sandbox driver crashed before returning a result: %w\nstderr: %s", err, tail)
	}
	return fmt.Errorf("codeexec: sandbox driver crashed before returning a result: %w", err)
}

func stringSize(v any) int {
	switch s := v.(type) {
	case string:
		return len(s)
	case []byte:
		return len(s)
	}
	return 0
}
