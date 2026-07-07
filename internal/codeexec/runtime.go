// Package codeexec runs model-authored Python in a monty sandbox, exposing
// host functions that call back into cc-context's tools. Only the distilled
// return value of a composition crosses back into the model's context,
// instead of every intermediate tool result. The sandbox lives in a
// uv-provisioned Python subprocess (driver.py) speaking JSON Lines; this
// package is the host side.
package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/format"
	"github.com/yasyf/cc-context/internal/render"
)

// runTimeout bounds one Run end to end: driver spawn (a cold uv cache
// downloads the wheel), the sandbox's own duration limit, and host calls.
const runTimeout = 3 * time.Minute

// Runtime executes scripts against a fixed registry of host functions.
type Runtime struct {
	funcs map[string]HostFunc
	stubs string
}

// NewRuntime builds a Runtime whose scripts can call the given host functions
// (awaited from Python). Independent awaits in one script run concurrently.
func NewRuntime(funcs map[string]HostFunc) *Runtime {
	return &Runtime{funcs: funcs, stubs: stubs(funcs)}
}

// Run compiles, typechecks, and executes script in a fresh driver subprocess,
// returning its rendered final value plus any captured stdout, trimmed to
// budget tokens. Compile, typecheck, and runtime failures are returned as the
// error so the caller can surface them for self-correction.
func (rt *Runtime) Run(ctx context.Context, script string, budget int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	d, err := launchDriver(ctx)
	if err != nil {
		return "", err
	}
	defer d.kill()
	// Closing stdin on cancellation trips the driver's EOF backstop, so the
	// stream unblocks even when the context kill only reaches uv.
	stop := context.AfterFunc(ctx, func() { _ = d.stdin.Close() })
	defer stop()

	init := initFrame{
		Script:    script,
		Stubs:     rt.stubs,
		Functions: slices.Sorted(maps.Keys(rt.funcs)),
		Limits:    map[string]any{"max_duration_secs": 120, "max_recursion_depth": 200},
	}
	done, err := pump(ctx, d.stdin, d.stdout, d.stderr.String, init, rt.funcs)
	if err != nil {
		return "", err
	}
	if !done.OK {
		return "", fmt.Errorf("%s: %s", done.Phase, done.Error)
	}
	val, err := decodeValue(done.Value)
	if err != nil {
		return "", fmt.Errorf("codeexec: decode result value: %w", err)
	}
	return render.Cap(rendered(val, done.Stdout), budget), nil
}

// stubs renders one untyped async stub per host function; untyped so the
// checker treats results as Any instead of guessing a return type.
func stubs(funcs map[string]HostFunc) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(funcs)) {
		fmt.Fprintf(&b, "async def %s(*args, **kwargs): ...\n", name)
	}
	return b.String()
}

func rendered(val any, stdout string) string {
	var b strings.Builder
	if stdout != "" {
		b.WriteString(stdout)
		if !strings.HasSuffix(stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	switch raw := val.(type) {
	case nil:
	case string:
		b.WriteString(raw)
	case []byte:
		b.Write(raw)
	case map[string]any, []any:
		b.WriteString(structured(val))
	default:
		if enc, err := json.Marshal(native(val)); err == nil {
			b.Write(enc)
		} else {
			fmt.Fprint(&b, raw)
		}
	}
	return b.String()
}

// structured renders a list/dict final value the way BashFormat renders JSON
// stdout: format.Convert picks the payload's leanest encoding via FormatAuto,
// with BashFormat's default indent and delimiter.
func structured(val any) string {
	enc, err := json.Marshal(native(val))
	if err != nil {
		return fmt.Sprint(val)
	}
	out, _, err := format.Convert(enc, format.Options{Format: format.FormatAuto, Indent: 2, Delimiter: format.DelimiterComma})
	if err != nil {
		return string(enc)
	}
	return out
}
