package codeexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/symbol"
	"github.com/yasyf/cc-context/internal/web"
)

// fakeDriver stands in for the Python child in pump tests: calls go out
// through send, and a drained results channel means dispatch writes can never
// block on the unbuffered pipe.
type fakeDriver struct {
	t       *testing.T
	enc     *json.Encoder
	results <-chan resultFrame
	close   func() // ends the driver's output stream, as a crash would
}

func (d *fakeDriver) send(frame map[string]any) {
	d.t.Helper()
	if err := d.enc.Encode(frame); err != nil {
		d.t.Errorf("fake driver send: %v", err)
	}
}

// runPump runs pump against an in-memory fake child: drive plays the driver's
// side while a resident reader drains every result frame into d.results.
func runPump(t *testing.T, funcs map[string]HostFunc, stderr func() string, drive func(d *fakeDriver)) (*driverFrame, error) {
	t.Helper()
	hostR, driverW := io.Pipe()
	driverR, hostW := io.Pipe()
	results := make(chan resultFrame, 64)
	go func() {
		dec := json.NewDecoder(driverR)
		var init initFrame
		if err := dec.Decode(&init); err != nil {
			close(results)
			return
		}
		for {
			var res resultFrame
			if err := dec.Decode(&res); err != nil {
				close(results)
				return
			}
			results <- res
		}
	}()
	d := &fakeDriver{t: t, enc: json.NewEncoder(driverW), results: results, close: func() { _ = driverW.Close() }}
	go drive(d)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = driverW.CloseWithError(ctx.Err()) })
	defer stop()

	done, err := pump(ctx, hostW, hostR, stderr, initFrame{Script: "test"}, funcs)
	_ = driverW.Close()
	_ = driverR.Close()
	_ = hostR.Close()
	_ = hostW.Close()
	return done, err
}

func noStderr() string { return "" }

// TestPumpParallelDispatch proves concurrent call frames run their host
// functions in parallel: every call blocks until all n are simultaneously
// active, so serial dispatch would deadlock into the pump timeout.
func TestPumpParallelDispatch(t *testing.T) {
	const n = 4
	var active, calls int32
	release := make(chan struct{})
	var once sync.Once
	funcs := map[string]HostFunc{
		"slow": func(ctx context.Context, _ Call) (any, error) {
			atomic.AddInt32(&calls, 1)
			if atomic.AddInt32(&active, 1) == n {
				once.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return int64(1), nil
		},
	}
	done, err := runPump(t, funcs, noStderr, func(d *fakeDriver) {
		for i := 1; i <= n; i++ {
			d.send(map[string]any{"t": "call", "id": i, "fn": "slow"})
		}
		sum := 0
		for range n {
			res := <-d.results
			if !res.OK {
				t.Errorf("host call failed: %s", res.Error)
			}
			var v int
			if err := json.Unmarshal(res.Value, &v); err != nil {
				t.Errorf("decode result value: %v", err)
			}
			sum += v
		}
		d.send(map[string]any{"t": "done", "ok": true, "value": sum})
	})
	if err != nil {
		t.Fatalf("pump error: %v", err)
	}
	if !done.OK || string(done.Value) != "4" {
		t.Errorf("done = ok %t value %s, want ok true value 4", done.OK, done.Value)
	}
	if got := atomic.LoadInt32(&calls); got != n {
		t.Errorf("host call invocations = %d, want %d", got, n)
	}
}

// TestErrCode proves the not-found sentinels map to the "not_found" wire code
// (through wrapping) and everything else to the empty code.
func TestErrCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"path not found", backend.ErrPathNotFound, "not_found"},
		{"wrapped path not found", fmt.Errorf("read: %w", backend.ErrPathNotFound), "not_found"},
		{"symbol not found", symbol.ErrNotFound, "not_found"},
		{"web gone", web.ErrGone, "not_found"},
		{"generic error", errors.New("boom"), ""},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errCode(tt.err); got != tt.want {
				t.Errorf("errCode(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestDispatchErrCode proves dispatch stamps the wire code onto a failed result
// frame (and leaves it empty on success), preserving the raw error message.
func TestDispatchErrCode(t *testing.T) {
	tests := []struct {
		name     string
		ret      any
		err      error
		wantOK   bool
		wantCode string
	}{
		{"path not found tags not_found", nil, backend.ErrPathNotFound, false, "not_found"},
		{"symbol not found tags not_found", nil, symbol.ErrNotFound, false, "not_found"},
		{"generic error no code", nil, errors.New("boom"), false, ""},
		{"success no code", "ok", nil, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			funcs := map[string]HostFunc{
				"fn": func(context.Context, Call) (any, error) { return tt.ret, tt.err },
			}
			res := dispatch(context.Background(), funcs, driverFrame{ID: 7, Fn: "fn"})
			if res.OK != tt.wantOK {
				t.Errorf("OK = %t, want %t", res.OK, tt.wantOK)
			}
			if res.ErrCode != tt.wantCode {
				t.Errorf("ErrCode = %q, want %q", res.ErrCode, tt.wantCode)
			}
			if res.ID != 7 {
				t.Errorf("ID = %d, want 7", res.ID)
			}
			if tt.err != nil && res.Error != tt.err.Error() {
				t.Errorf("Error = %q, want %q", res.Error, tt.err.Error())
			}
		})
	}
}

// TestPumpUnknownFunction proves a call to an unregistered function comes back
// ok:false naming it, without killing the run.
func TestPumpUnknownFunction(t *testing.T) {
	captured := make(chan resultFrame, 1)
	done, err := runPump(t, map[string]HostFunc{}, noStderr, func(d *fakeDriver) {
		d.send(map[string]any{"t": "call", "id": 1, "fn": "nope"})
		res := <-d.results
		captured <- res
		d.send(map[string]any{"t": "done", "ok": true, "value": nil})
	})
	if err != nil {
		t.Fatalf("pump error: %v", err)
	}
	if !done.OK {
		t.Errorf("done not ok: %s", done.Error)
	}
	res := <-captured
	if res.OK || !strings.Contains(res.Error, `no host function "nope"`) {
		t.Errorf("result = ok %t error %q, want ok false naming nope", res.OK, res.Error)
	}
}

// TestPumpDuplicateID proves a reused call id is an internal error, not a
// silently re-run host call.
func TestPumpDuplicateID(t *testing.T) {
	funcs := map[string]HostFunc{
		"fast": func(context.Context, Call) (any, error) { return int64(1), nil },
	}
	_, err := runPump(t, funcs, noStderr, func(d *fakeDriver) {
		d.send(map[string]any{"t": "call", "id": 1, "fn": "fast"})
		d.send(map[string]any{"t": "call", "id": 1, "fn": "fast"})
	})
	if err == nil {
		t.Fatal("pump = nil error, want duplicate id failure")
	}
	if !strings.Contains(err.Error(), "reused call id 1") {
		t.Errorf("error %q missing duplicate-id taxonomy", err)
	}
}

// TestPumpCrashCarriesStderr proves an EOF before the done frame surfaces the
// crash taxonomy with the driver's stderr tail.
func TestPumpCrashCarriesStderr(t *testing.T) {
	stderr := func() string { return "Traceback (most recent call last):\nRuntimeError: kaboom" }
	_, err := runPump(t, map[string]HostFunc{}, stderr, func(d *fakeDriver) {
		d.send(map[string]any{"t": "ready"})
		d.close() // stream ends without a done frame
	})
	if err == nil {
		t.Fatal("pump = nil error, want crash failure")
	}
	for _, want := range []string{"sandbox driver crashed", "RuntimeError: kaboom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestPumpAbandonsStuckHostCall proves a host call that ignores cancellation
// (a reflected third-party MCP tool) cannot wedge Run: once the stream dies,
// pump abandons the in-flight call at hostAbandonTimeout and returns.
func TestPumpAbandonsStuckHostCall(t *testing.T) {
	block := make(chan struct{})
	funcs := map[string]HostFunc{
		"stuck": func(context.Context, Call) (any, error) {
			<-block // ignores ctx on purpose
			return nil, nil
		},
	}
	hostR, driverW := io.Pipe()
	driverR, hostW := io.Pipe()
	t.Cleanup(func() {
		close(block) // unblock the abandoned goroutine so the test exits clean
		_ = hostW.Close()
		_ = hostR.Close()
		_ = driverR.Close()
	})
	go func() { _, _ = io.Copy(io.Discard, driverR) }()
	go func() {
		enc := json.NewEncoder(driverW)
		_ = enc.Encode(map[string]any{"t": "call", "id": 1, "fn": "stuck"})
		_ = driverW.CloseWithError(errors.New("driver died"))
	}()

	begin := time.Now()
	_, err := pump(context.Background(), hostW, hostR, noStderr, initFrame{Script: "test"}, funcs)
	if err == nil {
		t.Fatal("pump = nil error, want crash failure")
	}
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Errorf("pump returned after %v, want within ~%v of abandonment", elapsed, hostAbandonTimeout)
	}
}

// TestPumpValve proves an over-valve host return becomes the verbatim valve
// error frame instead of flooding the stream.
func TestPumpValve(t *testing.T) {
	funcs := map[string]HostFunc{
		"flood": func(context.Context, Call) (any, error) {
			return strings.Repeat("x", hostCallValve+1), nil
		},
	}
	captured := make(chan resultFrame, 1)
	if _, err := runPump(t, funcs, noStderr, func(d *fakeDriver) {
		d.send(map[string]any{"t": "call", "id": 1, "fn": "flood"})
		captured <- <-d.results
		d.send(map[string]any{"t": "done", "ok": true, "value": nil})
	}); err != nil {
		t.Fatalf("pump error: %v", err)
	}
	res := <-captured
	want := fmt.Sprintf(
		"codeexec valve: host call returned %d bytes (per-call limit %d); narrow the call with a tighter scope, section, or glob instead of reading it whole",
		hostCallValve+1, hostCallValve)
	if res.OK || res.Error != want {
		t.Errorf("result = ok %t error %q, want the verbatim valve message", res.OK, res.Error)
	}
}

// TestPumpValveStructured proves the valve also catches a structured return
// (slice/map) whose encoded size exceeds the limit — HostFunc returns any, so
// the raw-string pre-check alone would let it through.
func TestPumpValveStructured(t *testing.T) {
	funcs := map[string]HostFunc{
		"flood": func(context.Context, Call) (any, error) {
			return []any{strings.Repeat("x", hostCallValve+1)}, nil
		},
	}
	captured := make(chan resultFrame, 1)
	if _, err := runPump(t, funcs, noStderr, func(d *fakeDriver) {
		d.send(map[string]any{"t": "call", "id": 1, "fn": "flood"})
		captured <- <-d.results
		d.send(map[string]any{"t": "done", "ok": true, "value": nil})
	}); err != nil {
		t.Fatalf("pump error: %v", err)
	}
	res := <-captured
	if res.OK || !strings.Contains(res.Error, "codeexec valve") {
		t.Errorf("result = ok %t error %q, want the valve message", res.OK, res.Error)
	}
}
