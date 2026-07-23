package chunk

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/yasyf/cc-context/internal/cache"
)

const (
	// tsCallTimeout bounds one parse (instantiate + ts_parse). tsInitTimeout
	// bounds the one-time runtime build and each grammar's first cold compile,
	// held off the parse deadline so a slow first compile can never consume the
	// parse budget and silently downgrade a file to line chunking.
	tsCallTimeout = 30 * time.Second
	tsInitTimeout = 120 * time.Second
	// tsMaxMemoryPages caps an instance's linear memory at 1 GiB (64 KiB/page);
	// parsing a 1 MB file allocates well within this.
	tsMaxMemoryPages = 16384
)

// tsEngine is the process-wide wazero runtime and its per-language compiled
// grammar cache. A nil map entry marks a language with no embedded grammar.
type tsEngine struct {
	runtime  wazero.Runtime
	mu       sync.Mutex
	compiled map[string]wazero.CompiledModule
}

var (
	tsEngineMu   sync.Mutex
	tsEngineInst *tsEngine
)

// tsParser is the default parser: it drives the embedded tree-sitter grammars.
type tsParser struct{}

var defaultParser parser = tsParser{}

// parse returns the parse tree for lang, or ok=false to trigger line chunking —
// both when no grammar is embedded (expected, matching semble's unsupported
// languages) and when the WASM engine fails (logged; keeps chunking running).
func (tsParser) parse(lang string, src []byte) (node, bool) {
	root, ok, err := tsParse(lang, src)
	if err != nil {
		slog.Warn("semsearch/chunk: tree-sitter unavailable, falling back to line chunking",
			"language", lang, "error", err)
		return node{}, false
	}
	return root, ok
}

func tsParse(lang string, src []byte) (node, bool, error) {
	eng, err := loadTSEngine()
	if err != nil {
		return node{}, false, err
	}

	// A grammar's one-time cold compile runs under the generous init budget, not
	// the per-parse deadline; on subsequent parses compiledFor returns the cached
	// module and the wide budget is inert.
	compileCtx, cancelCompile := context.WithTimeout(context.Background(), tsInitTimeout)
	defer cancelCompile()
	compiled, ok, err := eng.compiledFor(compileCtx, lang)
	if err != nil {
		return node{}, false, err
	}
	if !ok {
		return node{}, false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), tsCallTimeout)
	defer cancel()
	mod, err := eng.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return node{}, false, fmt.Errorf("instantiate %s grammar: %w", lang, err)
	}
	defer func() { _ = mod.Close(ctx) }()
	if init := mod.ExportedFunction("_initialize"); init != nil {
		if _, err := init.Call(ctx); err != nil {
			return node{}, false, fmt.Errorf("initialize %s grammar: %w", lang, err)
		}
	}
	return runParse(ctx, mod, src)
}

// loadTSEngine builds the process-wide runtime on first use, caching only
// success so a transient cold-compile timeout is retried rather than pinned.
func loadTSEngine() (*tsEngine, error) {
	tsEngineMu.Lock()
	defer tsEngineMu.Unlock()
	if tsEngineInst != nil {
		return tsEngineInst, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), tsInitTimeout)
	defer cancel()

	dir, err := cache.Dir("wasm")
	if err != nil {
		return nil, fmt.Errorf("resolve wasm cache dir: %w", err)
	}
	compilationCache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, fmt.Errorf("open wasm compilation cache: %w", err)
	}
	rt, err := newTSCompilerRuntime(ctx, compilationCache)
	if err != nil {
		return nil, err
	}
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	tsEngineInst = &tsEngine{runtime: rt, compiled: make(map[string]wazero.CompiledModule)}
	return tsEngineInst, nil
}

// newTSCompilerRuntime builds the wazero compiler runtime, converting the panic
// NewRuntimeConfigCompiler raises on a backend-less host into an error (the
// interpreter is far too slow to parse whole repos).
func newTSCompilerRuntime(ctx context.Context, compilationCache wazero.CompilationCache) (rt wazero.Runtime, err error) {
	defer func() {
		if r := recover(); r != nil {
			rt, err = nil, fmt.Errorf(
				"wazero compiler backend unavailable on %s/%s: %v", runtime.GOOS, runtime.GOARCH, r)
		}
	}()
	return wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(compilationCache).
		WithMemoryLimitPages(tsMaxMemoryPages).
		WithCloseOnContextDone(true)), nil
}

// compileModule performs a grammar's one-time AOT compile. A package var so a
// test can observe the compile-scoped context, distinct from the parse deadline.
var compileModule = func(ctx context.Context, rt wazero.Runtime, wasm []byte) (wazero.CompiledModule, error) {
	return rt.CompileModule(ctx, wasm)
}

// compiledFor returns the compiled grammar for lang, compiling it on first use.
// ok is false with a nil error when no grammar is embedded for lang.
func (e *tsEngine) compiledFor(ctx context.Context, lang string) (wazero.CompiledModule, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if compiled, seen := e.compiled[lang]; seen {
		return compiled, compiled != nil, nil
	}

	gz, err := grammarFS.ReadFile("grammars/" + lang + ".wasm.gz")
	if err != nil {
		e.compiled[lang] = nil
		return nil, false, nil
	}
	wasmBytes, err := gunzip(gz)
	if err != nil {
		return nil, false, fmt.Errorf("decompress %s grammar: %w", lang, err)
	}
	compiled, err := compileModule(ctx, e.runtime, wasmBytes)
	if err != nil {
		return nil, false, fmt.Errorf("compile %s grammar: %w", lang, err)
	}
	e.compiled[lang] = compiled
	return compiled, true, nil
}

// runParse writes src into the instance, invokes ts_parse, and rebuilds the tree
// from the returned flat node array. The instance is closed by the caller, which
// reclaims both the source and result buffers, so neither is freed explicitly.
func runParse(ctx context.Context, mod api.Module, src []byte) (node, bool, error) {
	mem := mod.Memory()

	res, err := mod.ExportedFunction("ts_alloc").Call(ctx, uint64(len(src)))
	if err != nil {
		return node{}, false, fmt.Errorf("ts_alloc: %w", err)
	}
	ptr := uint32(res[0]) //nolint:gosec // wasm32: ts_alloc returns a 32-bit pointer
	if !mem.Write(ptr, src) {
		return node{}, false, fmt.Errorf("write source out of range at %d (%d bytes)", ptr, len(src))
	}

	packed, err := mod.ExportedFunction("ts_parse").Call(ctx, uint64(ptr), uint64(len(src)))
	if err != nil {
		return node{}, false, fmt.Errorf("ts_parse: %w", err)
	}
	outPtr, outLen := uint32(packed[0]>>32), uint32(packed[0]) //nolint:gosec // wasm32: ptr and len packed in one uint64
	buf, ok := mem.Read(outPtr, outLen)
	if !ok {
		return node{}, false, fmt.Errorf("read parse output out of range at %d (%d bytes)", outPtr, outLen)
	}
	return reconstructTree(buf), true, nil
}

// reconstructTree rebuilds the parse tree from bridge.c's flat buffer: a uint32
// node count followed by pre-order (start, end, child_count) triples. Descent is
// clamped at the chunker's recursionDepth guard: mergeNodeInner never reads a
// node's children below that depth, so deeper subtrees cannot affect any chunk
// and are skipped rather than materialized. This bounds the Go stack against a
// pathologically deep tree (e.g. hundreds of thousands of nested parens), which
// would otherwise exhaust it here before the chunker's own guard ever ran.
func reconstructTree(buf []byte) node {
	nodes := buf[4:]
	idx := 0
	// skipSubtrees advances idx past roots complete pre-order subtrees without
	// materializing them, iteratively so depth cannot exhaust the stack.
	skipSubtrees := func(roots uint32) {
		remaining := int(roots)
		for remaining > 0 {
			remaining += int(binary.LittleEndian.Uint32(nodes[idx*12+8:])) - 1
			idx++
		}
	}
	var build func(depth int) node
	build = func(depth int) node {
		base := idx * 12
		n := node{
			start: binary.LittleEndian.Uint32(nodes[base:]),
			end:   binary.LittleEndian.Uint32(nodes[base+4:]),
		}
		nchildren := binary.LittleEndian.Uint32(nodes[base+8:])
		idx++
		if nchildren == 0 {
			return n
		}
		if depth > recursionDepth {
			skipSubtrees(nchildren)
			return n
		}
		n.children = make([]node, nchildren)
		for i := range n.children {
			n.children[i] = build(depth + 1)
		}
		return n
	}
	return build(0)
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}
