package codeexec

import (
	"context"
	"fmt"

	monty "github.com/ewhauser/gomonty"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/search"
)

const defaultSnippetLines = 10

// Caller runs a single logical op and returns its output. *proxy.Proxy
// satisfies it; tests substitute a fake.
type Caller interface {
	Call(ctx context.Context, op backend.Op, a backend.Args) (string, error)
}

// Ops returns the ccx host functions a sandboxed script can await, each backed
// by one proxy op — the same query surface the MCP facade registers. The op
// output is returned uncapped (Budget left zero) so the script filters it in
// the sandbox; only the valve and the script's final value bound it.
func Ops(c Caller) map[string]HostFunc {
	op := func(op backend.Op, build func(*args) backend.Args) HostFunc {
		return func(ctx context.Context, call monty.Call) (monty.Value, error) {
			a := parse(call)
			ba := build(a)
			if a.err != nil {
				return monty.None(), a.err
			}
			out, err := c.Call(ctx, op, ba)
			if err != nil {
				return monty.None(), err
			}
			return monty.String(out), nil
		}
	}
	return map[string]HostFunc{
		"read": op(backend.OpRead, func(a *args) backend.Args {
			return backend.Args{Path: a.str("path", 0), Section: a.str("section", 1), Full: a.flag("full")}
		}),
		"grep": op(backend.OpGrep, func(a *args) backend.Args {
			return backend.Args{Query: a.str("text", 0), Glob: a.str("glob", 1), Expand: a.num("expand")}
		}),
		"symbol": op(backend.OpSymbol, func(a *args) backend.Args {
			return backend.Args{Query: a.str("name", 0), Scope: a.str("scope", 1), Full: a.flag("full")}
		}),
		"find":    op(backend.OpFind, func(a *args) backend.Args { return backend.Args{Glob: a.str("glob", 0), Scope: a.str("scope", 1)} }),
		"related": op(backend.OpRelated, func(a *args) backend.Args { return backend.Args{Query: a.str("location", 0)} }),
		"deps":    op(backend.OpDeps, func(a *args) backend.Args { return backend.Args{Path: a.str("path", 0), Scope: a.str("scope", 1)} }),
		"diff": op(backend.OpDiff, func(a *args) backend.Args {
			return backend.Args{Source: a.strOr("source", 0, "uncommitted"), Scope: a.str("scope", 1)}
		}),
		"overview": op(backend.OpOverview, func(*args) backend.Args { return backend.Args{} }),
		"search":   routed(c, func(a backend.Args) (backend.Op, error) { op, _, err := search.Route(a); return op, err }, searchArgs),
		"outline":  routed(c, outline.Route, outlineArgs),
	}
}

func searchArgs(a *args) backend.Args {
	return backend.Args{
		Query:           a.str("query", 0),
		Path:            a.str("repo", -1),
		Mode:            a.str("mode", -1),
		Lang:            a.str("lang", -1),
		K:               a.num("k"),
		MaxSnippetLines: defaultSnippetLines,
	}
}

func outlineArgs(a *args) backend.Args {
	return backend.Args{Path: a.str("path", 0), Items: a.str("items", -1), Match: a.str("match", -1), Lang: a.str("lang", -1)}
}

// routed builds a host function whose op is chosen at call time by a router
// (search and outline classify their input before dispatch).
func routed(c Caller, route func(backend.Args) (backend.Op, error), build func(*args) backend.Args) HostFunc {
	return func(ctx context.Context, call monty.Call) (monty.Value, error) {
		p := parse(call)
		a := build(p)
		if p.err != nil {
			return monty.None(), p.err
		}
		op, err := route(a)
		if err != nil {
			return monty.None(), err
		}
		if op == backend.OpStructural && a.Path != "" {
			a.Paths = []string{a.Path}
		}
		out, err := c.Call(ctx, op, a)
		if err != nil {
			return monty.None(), err
		}
		return monty.String(out), nil
	}
}

// args reads positional and keyword arguments from a monty call, recording the
// first mapping failure in err. A negative index means keyword-only.
type args struct {
	pos []monty.Value
	kw  map[string]monty.Value
	err error
}

func parse(call monty.Call) *args {
	kw := make(map[string]monty.Value, len(call.Kwargs))
	for _, p := range call.Kwargs {
		if k, ok := p.Key.Raw().(string); ok {
			kw[k] = p.Value
		}
	}
	return &args{pos: call.Args, kw: kw}
}

func (a *args) val(name string, idx int) (monty.Value, bool) {
	if v, ok := a.kw[name]; ok {
		return v, true
	}
	if idx >= 0 && idx < len(a.pos) {
		return a.pos[idx], true
	}
	return monty.Value{}, false
}

func (a *args) str(name string, idx int) string {
	if v, ok := a.val(name, idx); ok {
		if s, ok := v.Raw().(string); ok {
			return s
		}
	}
	return ""
}

func (a *args) strOr(name string, idx int, def string) string {
	if s := a.str(name, idx); s != "" {
		return s
	}
	return def
}

func (a *args) flag(name string) bool {
	if v, ok := a.val(name, -1); ok {
		b, _ := v.Raw().(bool)
		return b
	}
	return false
}

// num reads a keyword-only numeric argument. An absent argument defaults to
// zero; a present None or non-numeric value records a labeled error so the
// sandbox sees the mistake instead of a silent zero.
func (a *args) num(name string) int {
	v, ok := a.val(name, -1)
	if !ok {
		return 0
	}
	switch n := v.Raw().(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		a.fail(fmt.Errorf("codeexec: argument %q must be a number, got %s", name, v.Kind()))
		return 0
	}
}

func (a *args) fail(err error) {
	if a.err == nil {
		a.err = err
	}
}
