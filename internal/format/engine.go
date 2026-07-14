package format

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/yasyf/cc-context/internal/cache"
)

// Built from format-core/wasm by `task wasm` and copied here; gitignored, so a
// source build must run the wasm task first.
//
//go:embed formatcore.wasm
var wasmModule []byte

const (
	// callTimeout bounds one fc_format invocation; warm calls are sub-ms.
	callTimeout = 10 * time.Second
	// initTimeout bounds the one-time compile on its own budget, never the
	// caller's callTimeout: a warm-machine cold-cache compile measured ~17ms, so
	// this is pure headroom for a CPU-starved CI host.
	initTimeout = 60 * time.Second
	// maxMemoryPages caps a WASM instance's linear memory at 1 GiB (64 KiB/page).
	maxMemoryPages = 16384
)

// engine is the lazily-compiled runtime shared across calls. Each format call
// instantiates its own one-shot module — the ABI leaks per-call allocations.
type engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

var (
	engineMu   sync.Mutex
	engineInst *engine
)

// errEngineUnavailable marks a load/compile failure (e.g. the compiler-backend
// guard) — always loud, unlike a per-call trap that follows passthrough policy.
var errEngineUnavailable = errors.New("format engine unavailable")

// loadEngine returns the process-wide engine, compiling it on first use under a
// dedicated initTimeout budget. It caches only success: a failed init leaves
// engineInst nil and returns the error uncached, so the next call retries. A
// plain sync.Once is the wrong primitive here — pinning whatever the first init
// returns would lock a transient cold-compile timeout into errEngineUnavailable
// for the process lifetime, fatal for the long-lived MCP server.
func loadEngine() (*engine, error) {
	engineMu.Lock()
	defer engineMu.Unlock()
	if engineInst != nil {
		return engineInst, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()

	eng, err := initEngine(ctx)
	if err != nil {
		return nil, err
	}
	engineInst = eng
	return eng, nil
}

// newCompilerRuntime builds the wazero compiler runtime. NewRuntimeConfigCompiler
// panics on a host without a compiler backend rather than falling back to the
// interpreter (~18x over budget); the recover is scoped to exactly that
// construction so any unrelated panic propagates.
func newCompilerRuntime(ctx context.Context, compilationCache wazero.CompilationCache) (rt wazero.Runtime, err error) {
	defer func() {
		if r := recover(); r != nil {
			rt, err = nil, fmt.Errorf(
				"wazero compiler backend unavailable on %s/%s (interpreter is ~18x over budget): %v",
				runtime.GOOS, runtime.GOARCH, r,
			)
		}
	}()
	return wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(compilationCache).
		WithMemoryLimitPages(maxMemoryPages).
		WithCloseOnContextDone(true)), nil
}

// initEngine resolves the on-disk compilation cache and compiles the module
// behind wazero's compiler backend.
func initEngine(ctx context.Context) (*engine, error) {
	dir, err := cache.Dir("wasm")
	if err != nil {
		return nil, fmt.Errorf("resolve wasm cache dir: %w", err)
	}
	compilationCache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, fmt.Errorf("open wasm compilation cache: %w", err)
	}

	rt, err := newCompilerRuntime(ctx, compilationCache)
	if err != nil {
		return nil, err
	}

	compiled, err := rt.CompileModule(ctx, wasmModule)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compile formatcore.wasm: %w", err)
	}
	return &engine{runtime: rt, compiled: compiled}, nil
}

// engineResult is the parsed response: a chosen encoding, or a domain error
// keyed by the envelope's stable `kind` (a host failure is runEngine's error).
type engineResult struct {
	format  Format
	text    string
	errKind string
	errMsg  string
}

type wasmRequest struct {
	Src       string  `json:"src"`
	Format    *string `json:"format"`
	Indent    int     `json:"indent"`
	Delimiter string  `json:"delimiter"`
}

type wasmResponse struct {
	OK *struct {
		Format string `json:"format"`
		Text   string `json:"text"`
	} `json:"ok"`
	Err *struct {
		Kind string `json:"kind"`
		Msg  string `json:"msg"`
	} `json:"err"`
}

// runEngine runs src and opts through one one-shot module instance. The error
// return is a host failure (trap/timeout/limit); a domain error rides in errKind.
func runEngine(src []byte, opts Options) (engineResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	eng, err := loadEngine()
	if err != nil {
		return engineResult{}, errors.Join(errEngineUnavailable, err)
	}

	req := wasmRequest{Src: string(src), Indent: opts.Indent, Delimiter: opts.Delimiter.char()}
	if opts.Format != FormatAuto {
		name := string(opts.Format)
		req.Format = &name
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return engineResult{}, fmt.Errorf("marshal wasm request: %w", err)
	}

	mod, err := eng.runtime.InstantiateModule(ctx, eng.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return engineResult{}, fmt.Errorf("instantiate formatcore.wasm: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	respBytes, err := callFormat(ctx, mod, reqBytes)
	if err != nil {
		return engineResult{}, err
	}

	var resp wasmResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return engineResult{}, fmt.Errorf("decode wasm response %q: %w", respBytes, err)
	}
	switch {
	case resp.OK != nil:
		return engineResult{format: Format(resp.OK.Format), text: resp.OK.Text}, nil
	case resp.Err != nil:
		return engineResult{errKind: resp.Err.Kind, errMsg: resp.Err.Msg}, nil
	default:
		return engineResult{}, fmt.Errorf("wasm response had neither ok nor err: %q", respBytes)
	}
}

// callFormat runs the fc_alloc → write → fc_format → read dance and copies the
// response out (the read aliases WASM memory the instance reclaims on close).
func callFormat(ctx context.Context, mod api.Module, req []byte) ([]byte, error) {
	mem := mod.Memory()

	alloc, err := mod.ExportedFunction("fc_alloc").Call(ctx, uint64(len(req)))
	if err != nil {
		return nil, fmt.Errorf("wasm fc_alloc: %w", err)
	}
	ptr := uint32(alloc[0]) //nolint:gosec // wasm32: fc_alloc returns a 32-bit pointer in the low half
	if !mem.Write(ptr, req) {
		return nil, fmt.Errorf("wasm memory write out of range at %d (%d bytes)", ptr, len(req))
	}

	packed, err := mod.ExportedFunction("fc_format").Call(ctx, uint64(ptr), uint64(len(req)))
	if err != nil {
		return nil, fmt.Errorf("wasm fc_format: %w", err)
	}
	outPtr, outLen := uint32(packed[0]>>32), uint32(packed[0]) //nolint:gosec // wasm32: fc_format packs ptr and len into one uint64
	out, ok := mem.Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("wasm memory read out of range at %d (%d bytes)", outPtr, outLen)
	}
	return append([]byte(nil), out...), nil
}
