package codeexec

import (
	"context"
	"reflect"
	"strings"
	"testing"

	monty "github.com/ewhauser/gomonty"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/proxy"
)

var _ Caller = (*proxy.Proxy)(nil)

type fakeCaller struct {
	op   backend.Op
	args backend.Args
	out  string
}

func (f *fakeCaller) Call(_ context.Context, op backend.Op, a backend.Args) (string, error) {
	f.op, f.args = op, a
	return f.out, nil
}

func kw(pairs ...string) monty.Dict {
	d := make(monty.Dict, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		d = append(d, monty.Pair{Key: monty.String(pairs[i]), Value: monty.String(pairs[i+1])})
	}
	return d
}

func TestOpsArgMapping(t *testing.T) {
	tests := []struct {
		name    string
		fn      string
		call    monty.Call
		wantOp  backend.Op
		wantArg backend.Args
	}{
		{
			"read kwargs", "read",
			monty.Call{Kwargs: kw("path", "foo.go", "section", "10-20")},
			backend.OpRead,
			backend.Args{Path: "foo.go", Section: "10-20"},
		},
		{
			"read positional", "read",
			monty.Call{Args: []monty.Value{monty.String("bar.go")}},
			backend.OpRead,
			backend.Args{Path: "bar.go"},
		},
		{
			"read full flag", "read",
			monty.Call{Kwargs: monty.Dict{{Key: monty.String("path"), Value: monty.String("z.go")}, {Key: monty.String("full"), Value: monty.Bool(true)}}},
			backend.OpRead,
			backend.Args{Path: "z.go", Full: true},
		},
		{
			"grep", "grep",
			monty.Call{Kwargs: kw("text", "RunDiffCLI", "glob", "*.go")},
			backend.OpGrep,
			backend.Args{Query: "RunDiffCLI", Glob: "*.go"},
		},
		{
			"grep expand int", "grep",
			monty.Call{Kwargs: monty.Dict{{Key: monty.String("text"), Value: monty.String("x")}, {Key: monty.String("expand"), Value: monty.Int(3)}}},
			backend.OpGrep,
			backend.Args{Query: "x", Expand: 3},
		},
		{
			"grep expand float", "grep",
			monty.Call{Kwargs: monty.Dict{{Key: monty.String("text"), Value: monty.String("x")}, {Key: monty.String("expand"), Value: monty.Float(2.0)}}},
			backend.OpGrep,
			backend.Args{Query: "x", Expand: 2},
		},
		{
			"symbol", "symbol",
			monty.Call{Kwargs: kw("name", "Cap", "scope", "internal/render")},
			backend.OpSymbol,
			backend.Args{Query: "Cap", Scope: "internal/render"},
		},
		{
			"find", "find",
			monty.Call{Kwargs: kw("glob", "**/*.go")},
			backend.OpFind,
			backend.Args{Glob: "**/*.go"},
		},
		{
			"diff default source", "diff",
			monty.Call{},
			backend.OpDiff,
			backend.Args{Source: "uncommitted"},
		},
		{
			"diff override", "diff",
			monty.Call{Kwargs: kw("source", "staged")},
			backend.OpDiff,
			backend.Args{Source: "staged"},
		},
		{
			"search literal routes to grep", "search",
			monty.Call{Kwargs: kw("query", "needle", "mode", "literal")},
			backend.OpGrep,
			backend.Args{Query: "needle", Mode: "literal", MaxSnippetLines: defaultSnippetLines},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCaller{out: "ok"}
			fn := Ops(fake)[tt.fn]
			if fn == nil {
				t.Fatalf("no host function %q", tt.fn)
			}
			val, err := fn(context.Background(), tt.call)
			if err != nil {
				t.Fatalf("call error: %v", err)
			}
			if got := val.Raw().(string); got != "ok" {
				t.Errorf("value = %q, want %q", got, "ok")
			}
			if fake.op != tt.wantOp {
				t.Errorf("op = %q, want %q", fake.op, tt.wantOp)
			}
			if !reflect.DeepEqual(fake.args, tt.wantArg) {
				t.Errorf("args = %+v, want %+v", fake.args, tt.wantArg)
			}
		})
	}
}

// TestOpsNumArgErrors proves a None or type-mismatched numeric argument raises
// a labeled error instead of silently mapping to zero.
func TestOpsNumArgErrors(t *testing.T) {
	tests := []struct {
		name string
		fn   string
		call monty.Call
	}{
		{
			"grep expand none", "grep",
			monty.Call{Kwargs: monty.Dict{{Key: monty.String("text"), Value: monty.String("x")}, {Key: monty.String("expand"), Value: monty.None()}}},
		},
		{
			"grep expand string", "grep",
			monty.Call{Kwargs: kw("text", "x", "expand", "5")},
		},
		{
			"search k none", "search",
			monty.Call{Kwargs: monty.Dict{{Key: monty.String("query"), Value: monty.String("q")}, {Key: monty.String("mode"), Value: monty.String("literal")}, {Key: monty.String("k"), Value: monty.None()}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCaller{out: "ok"}
			_, err := Ops(fake)[tt.fn](context.Background(), tt.call)
			if err == nil {
				t.Fatal("call error = nil, want labeled argument error")
			}
			for _, want := range []string{"codeexec:", "must be a number"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if fake.op != "" {
				t.Errorf("op %q dispatched despite argument error", fake.op)
			}
		})
	}
}

// TestOpsNumArgErrorRaises proves the labeled argument error crosses into the
// sandbox as a raised exception, not a zero value.
func TestOpsNumArgErrorRaises(t *testing.T) {
	rt := NewRuntime(Ops(&fakeCaller{out: "ok"}))
	_, err := rt.Run(context.Background(), "import asyncio\nasyncio.run(grep(\"x\", expand=None))", 0)
	if err == nil {
		t.Fatal("Run = nil error, want raised argument error")
	}
	for _, want := range []string{"codeexec:", `"expand"`, "must be a number"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestOpsThroughRuntime proves Python kwargs flow through monty into backend.Args.
func TestOpsThroughRuntime(t *testing.T) {
	fake := &fakeCaller{out: "FILE BODY"}
	rt := NewRuntime(Ops(fake))
	got, err := rt.Run(context.Background(), "import asyncio\nasyncio.run(read(path=\"x.go\", section=\"5-9\"))", 0)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if got != "FILE BODY" {
		t.Errorf("got %q, want %q", got, "FILE BODY")
	}
	if fake.op != backend.OpRead || fake.args.Path != "x.go" || fake.args.Section != "5-9" {
		t.Errorf("recorded op=%q args=%+v", fake.op, fake.args)
	}
}
