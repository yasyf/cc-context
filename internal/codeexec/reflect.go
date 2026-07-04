package codeexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	monty "github.com/ewhauser/gomonty"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/mcpclient"
	"github.com/yasyf/cc-context/internal/version"
)

// connectFunc opens a live session to one discovered server; the engine
// injects it so tests can substitute in-memory transports.
type connectFunc func(ctx context.Context, spec ServerSpec) (*mcp.ClientSession, error)

func defaultConnect(ctx context.Context, spec ServerSpec) (*mcp.ClientSession, error) {
	if spec.URL != "" {
		client := mcp.NewClient(&mcp.Implementation{Name: "cc-context-codeexec", Version: version.String()}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: spec.URL}, nil)
	}
	return mcpclient.Connect(ctx, "cc-context-codeexec", spec.Command, spec.Argv...)
}

// lazyConn guards one server's resident session. Its own mutex serializes that
// server's first connect without blocking a different server's.
type lazyConn struct {
	mu      sync.Mutex
	spec    ServerSpec
	session *mcp.ClientSession
}

func (lc *lazyConn) close() error {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.session == nil {
		return nil
	}
	err := lc.session.Close()
	lc.session = nil
	return err
}

// Reflector turns a catalog's tools into sandbox host functions, opening each
// server's session lazily on the first call to one of its tools and at most
// once per server.
type Reflector struct {
	connect connectFunc
	prewarm sync.WaitGroup

	mu    sync.Mutex
	conns map[string]*lazyConn
	tools []ToolInfo
}

// NewReflector returns a Reflector with no sessions and no catalog yet.
func NewReflector(connect connectFunc) *Reflector {
	return &Reflector{connect: connect, conns: map[string]*lazyConn{}}
}

// SetCatalog swaps in cat's tool surface. A server still present with the same
// launch spec keeps its live session; servers that dropped out (or relaunched
// differently) have theirs closed, so a rebuild never orphans a child process.
func (r *Reflector) SetCatalog(cat *Catalog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]*lazyConn, len(cat.Servers))
	r.tools = nil
	for _, sc := range cat.Servers {
		lc, ok := r.conns[sc.Spec.Name]
		if !ok || lc.spec.commandLine() != sc.Spec.commandLine() {
			lc = &lazyConn{spec: sc.Spec}
		}
		next[sc.Spec.Name] = lc
		r.tools = append(r.tools, sc.Tools...)
	}
	var errs []error
	for name, lc := range r.conns {
		if next[name] != lc {
			errs = append(errs, lc.close())
		}
	}
	r.conns = next
	return errors.Join(errs...)
}

// Funcs returns one host function per catalog tool, keyed by FuncName.
func (r *Reflector) Funcs() map[string]HostFunc {
	r.mu.Lock()
	defer r.mu.Unlock()
	funcs := make(map[string]HostFunc, len(r.tools))
	for _, t := range r.tools {
		funcs[t.FuncName] = r.hostFunc(t)
	}
	return funcs
}

// hostFunc calls one reflected tool: kwargs only (positional args raise a
// labeled error naming the function), transport and IsError failures raise
// into the sandbox, and the result's text returns as a string so the per-call
// valve applies.
func (r *Reflector) hostFunc(t ToolInfo) HostFunc {
	return func(ctx context.Context, call monty.Call) (monty.Value, error) {
		if len(call.Args) > 0 {
			return monty.None(), fmt.Errorf("codeexec: %s takes keyword arguments only — call it as %s(%s)", t.FuncName, t.FuncName, kwargHint(t.Params))
		}
		session, err := r.session(ctx, t.Server)
		if err != nil {
			return monty.None(), err
		}
		args := make(map[string]any, len(call.Kwargs))
		for _, p := range call.Kwargs {
			args[p.Key.Raw().(string)] = native(p.Value)
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: t.Tool, Arguments: args})
		if err != nil {
			return monty.None(), fmt.Errorf("codeexec: %s: call %q on %s: %w", t.FuncName, t.Tool, t.Server, err)
		}
		text := mcpclient.TextOf(res)
		if res.IsError {
			return monty.None(), fmt.Errorf("codeexec: %s: tool %q on %s failed: %s", t.FuncName, t.Tool, t.Server, text)
		}
		return monty.String(text), nil
	}
}

// session returns the server's resident session, connecting on first use. The
// connection outlives the triggering call (context.WithoutCancel) so it stays
// resident; a failed connect is not cached, so the next call retries.
func (r *Reflector) session(ctx context.Context, server string) (*mcp.ClientSession, error) {
	r.mu.Lock()
	lc, ok := r.conns[server]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("codeexec: no reflected server %q", server)
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.session != nil {
		return lc.session, nil
	}
	session, err := r.connect(context.WithoutCancel(ctx), lc.spec)
	if err != nil {
		return nil, fmt.Errorf("codeexec: connect %s: %w", server, err)
	}
	lc.session = session
	return session, nil
}

// Prewarm opens sessions for the named servers concurrently, best-effort: a
// failure here just defers the error to the first real call.
func (r *Reflector) Prewarm(ctx context.Context, servers []string) {
	for _, name := range servers {
		r.prewarm.Add(1)
		go func() {
			defer r.prewarm.Done()
			_, _ = r.session(ctx, name)
		}()
	}
}

// Sigs renders the catalog tools for the Preamble: kwargs-only signatures, a
// [writes] label on anything not explicitly read-only, and the origin server.
func (r *Reflector) Sigs() []ToolSig {
	r.mu.Lock()
	defer r.mu.Unlock()
	sigs := make([]ToolSig, len(r.tools))
	for i, t := range r.tools {
		sig := fmt.Sprintf("%s(%s)", t.FuncName, kwargHint(t.Params))
		if !t.ReadOnly {
			sig += " [writes]"
		}
		summary := t.Summary
		if summary == "" {
			summary = t.Tool
		}
		sigs[i] = ToolSig{Name: t.FuncName, Signature: sig, Summary: fmt.Sprintf("[%s] %s", t.Server, summary)}
	}
	return sigs
}

// FuncServers maps every reflected function name to its server name, for the
// pre-warm scan.
func (r *Reflector) FuncServers() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]string, len(r.tools))
	for _, t := range r.tools {
		m[t.FuncName] = t.Server
	}
	return m
}

// Close waits out in-flight pre-warms and tears down every open session.
func (r *Reflector) Close() error {
	r.prewarm.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	errs := make([]error, 0, len(r.conns))
	for _, lc := range r.conns {
		errs = append(errs, lc.close())
	}
	r.conns = map[string]*lazyConn{}
	return errors.Join(errs...)
}

func kwargHint(params []string) string {
	hints := make([]string, len(params))
	for i, p := range params {
		hints[i] = p + "=..."
	}
	return strings.Join(hints, ", ")
}
