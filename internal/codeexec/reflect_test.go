package codeexec

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func fakeCatalog() *Catalog {
	return &Catalog{
		Hash: "h1",
		Servers: []ServerCatalog{{
			Spec: ServerSpec{Name: "fake", Command: "fake-mcp", Prefix: "fake"},
			Tools: []ToolInfo{
				{Server: "fake", Tool: "echo", FuncName: "fake_echo", Params: []string{"text"}, Summary: "Echoes.", ReadOnly: true},
				{Server: "fake", Tool: "boom", FuncName: "fake_boom", Summary: "Fails."},
			},
		}},
	}
}

func kwargs(kv map[string]any) Call {
	return Call{Kwargs: kv}
}

func callTool(t *testing.T, funcs map[string]HostFunc, name string, call Call) (string, error) {
	t.Helper()
	fn, ok := funcs[name]
	if !ok {
		t.Fatalf("host function %q not registered (have %v)", name, sortedKeys(funcs))
	}
	val, err := fn(context.Background(), call)
	if err != nil {
		return "", err
	}
	text, ok := val.(string)
	if !ok {
		t.Fatalf("host function %q returned %T, want string", name, val)
	}
	return text, nil
}

func sortedKeys(funcs map[string]HostFunc) []string {
	keys := make([]string, 0, len(funcs))
	for k := range funcs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestReflectorLazyConnect(t *testing.T) {
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	r := NewReflector(conn.connect)
	t.Cleanup(func() { _ = r.Close() })

	if err := r.SetCatalog(fakeCatalog()); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}
	funcs := r.Funcs()
	if want := []string{"fake_boom", "fake_echo"}; !reflect.DeepEqual(sortedKeys(funcs), want) {
		t.Fatalf("Funcs = %v, want %v", sortedKeys(funcs), want)
	}
	if n := conn.connectCount("fake"); n != 0 {
		t.Fatalf("connects before first call = %d, want 0", n)
	}

	for range 2 {
		out, err := callTool(t, funcs, "fake_echo", kwargs(map[string]any{"text": "hi"}))
		if err != nil {
			t.Fatalf("fake_echo: %v", err)
		}
		if out != `{"text":"hi"}` {
			t.Errorf("fake_echo = %q, want %q", out, `{"text":"hi"}`)
		}
	}
	if n := conn.connectCount("fake"); n != 1 {
		t.Errorf("connects after two calls = %d, want 1", n)
	}
}

func TestReflectorConcurrentFirstCall(t *testing.T) {
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	r := NewReflector(conn.connect)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.SetCatalog(fakeCatalog()); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}
	fn := r.Funcs()["fake_echo"]

	const callers = 8
	var wg sync.WaitGroup
	outs := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := fn(context.Background(), kwargs(map[string]any{"text": "hi"}))
			if err != nil {
				errs[i] = err
				return
			}
			outs[i], _ = val.(string)
		}()
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if want := `{"text":"hi"}`; outs[i] != want {
			t.Errorf("caller %d = %q, want %q", i, outs[i], want)
		}
	}
	if n := conn.connectCount("fake"); n != 1 {
		t.Errorf("concurrent first calls connected %d times, want 1", n)
	}
}

func TestReflectorKwargsNested(t *testing.T) {
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	r := NewReflector(conn.connect)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.SetCatalog(fakeCatalog()); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}

	call := kwargs(map[string]any{
		"text": "hi",
		"meta": map[string]any{"k": []any{int64(1), int64(2)}, "flag": true},
	})
	out, err := callTool(t, r.Funcs(), "fake_echo", call)
	if err != nil {
		t.Fatalf("fake_echo: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	want := map[string]any{
		"text": "hi",
		"meta": map[string]any{"k": []any{1.0, 2.0}, "flag": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("arguments = %#v, want %#v", got, want)
	}
}

func TestReflectorErrors(t *testing.T) {
	tests := []struct {
		name string
		fn   string
		call Call
		want []string
	}{
		{
			"positional args rejected",
			"fake_echo",
			Call{Args: []any{"hi"}},
			[]string{"fake_echo takes keyword arguments only", "fake_echo(text=...)"},
		},
		{
			"IsError raised",
			"fake_boom",
			kwargs(nil),
			[]string{`tool "boom" on fake failed`, "kaboom"},
		},
	}
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	r := NewReflector(conn.connect)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.SetCatalog(fakeCatalog()); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}
	funcs := r.Funcs()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := callTool(t, funcs, tt.fn, tt.call)
			if err == nil {
				t.Fatalf("%s = nil error, want failure", tt.fn)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestReflectorConnectErrorLabeled(t *testing.T) {
	conn := newFakeConnector()
	conn.fail["fake"] = true
	r := NewReflector(conn.connect)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.SetCatalog(fakeCatalog()); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}

	_, err := callTool(t, r.Funcs(), "fake_echo", kwargs(map[string]any{"text": "x"}))
	if err == nil {
		t.Fatal("want connect error, got nil")
	}
	for _, want := range []string{"connect fake", "dial refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestReflectorOrphanCleanup(t *testing.T) {
	ctx := context.Background()
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	conn.servers["other"] = echoServer()
	r := NewReflector(conn.connect)
	t.Cleanup(func() { _ = r.Close() })

	full := fakeCatalog()
	full.Servers = append(full.Servers, ServerCatalog{
		Spec:  ServerSpec{Name: "other", Command: "other-mcp", Prefix: "other"},
		Tools: []ToolInfo{{Server: "other", Tool: "echo", FuncName: "other_echo", Params: []string{"text"}}},
	})
	if err := r.SetCatalog(full); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}
	funcs := r.Funcs()
	for _, fn := range []string{"fake_echo", "other_echo"} {
		if _, err := callTool(t, funcs, fn, kwargs(map[string]any{"text": "x"})); err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
	}

	rebuilt := &Catalog{Hash: "h2", Servers: full.Servers[1:]}
	if err := r.SetCatalog(rebuilt); err != nil {
		t.Fatalf("SetCatalog rebuild: %v", err)
	}
	if _, ok := r.Funcs()["fake_echo"]; ok {
		t.Error("fake_echo still registered after its server dropped out")
	}
	if err := conn.session(t, "fake", 0).Ping(ctx, nil); err == nil {
		t.Error("dropped server's session still alive, want closed")
	}
	if err := conn.session(t, "other", 0).Ping(ctx, nil); err != nil {
		t.Errorf("surviving server's session closed: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.session(t, "other", 0).Ping(ctx, nil); err == nil {
		t.Error("session still alive after Close")
	}
}

func TestReflectorSigs(t *testing.T) {
	r := NewReflector(nil)
	if err := r.SetCatalog(fakeCatalog()); err != nil {
		t.Fatalf("SetCatalog: %v", err)
	}
	want := []ToolSig{
		{Name: "fake_echo", Signature: "fake_echo(text=...)", Summary: "[fake] Echoes."},
		{Name: "fake_boom", Signature: "fake_boom() [writes]", Summary: "[fake] Fails."},
	}
	if got := r.Sigs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Sigs = %+v, want %+v", got, want)
	}
}
