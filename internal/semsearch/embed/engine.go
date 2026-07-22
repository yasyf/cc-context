// Package embed is a resident model2vec inference engine. It loads
// minishlab/potion-code-16M-v2 once per process into a wazero-instantiated WASM
// module (the model2vec-rs crate compiled to wasm32-unknown-unknown, keeping
// tokenizer parity with Python model2vec) and encodes batches of raw text into
// L2-normalized float32 vectors.
//
// Unlike internal/format's per-call instantiation, the module here is
// resident: New instantiates it and decodes the ~30 MB of weights into its
// linear memory exactly once, and every Encode reuses that instance. That
// shared memory means one Engine serializes its Encode calls behind a mutex —
// the work is memory-bound, so a caller scales with more Engine instances, not
// concurrent calls on one.
package embed

import (
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/yasyf/cc-context/internal/cache"
)

// Built from embed-core/wasm by `task wasm-embed` and copied here; gitignored,
// so a source build must run the wasm-embed task first.
//
//go:embed embedcore.wasm
var wasmModule []byte

const (
	// compileTimeout bounds the one-time module compile; a warm-cache AOT load
	// is sub-100ms, so this is headroom for a cold compile on a slow host.
	compileTimeout = 60 * time.Second
	// loadTimeout bounds em_load_model decoding the weights into linear memory.
	loadTimeout = 60 * time.Second
	// maxMemoryPages caps the instance's linear memory at 1 GiB (64 KiB/page);
	// the decoded model needs ~150 MB, so this is pure headroom.
	maxMemoryPages = 16384
)

// Engine is a resident model2vec inference engine. Construct it with New and
// release its WASM instance with Close. Encode is safe for concurrent use;
// calls serialize on a single shared instance.
type Engine struct {
	runtime   wazero.Runtime
	module    api.Module
	alloc     api.Function
	dealloc   api.Function
	loadModel api.Function
	encode    api.Function

	mu   sync.Mutex
	dims int
}

// New resolves the pinned weights (downloading once into the cache), compiles
// the WASM module behind wazero's compiler backend, instantiates it resident,
// and loads the weights into its linear memory. It returns ErrWeightsUnavailable
// when the weights are neither cached nor downloadable.
func New(ctx context.Context) (*Engine, error) {
	blobs, err := resolveWeights(ctx)
	if err != nil {
		return nil, err
	}

	rt, compiled, err := compileModule(ctx)
	if err != nil {
		return nil, err
	}

	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiate embedcore.wasm: %w", err)
	}

	e := &Engine{
		runtime:   rt,
		module:    mod,
		alloc:     mod.ExportedFunction("em_alloc"),
		dealloc:   mod.ExportedFunction("em_dealloc"),
		loadModel: mod.ExportedFunction("em_load_model"),
		encode:    mod.ExportedFunction("em_encode"),
	}

	if err := e.load(ctx, blobs); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}

	// Warm up: JITs the encode path and captures the embedding dimensionality.
	warm, err := e.encodeBatch(ctx, []string{"warm up"})
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("warmup encode: %w", err)
	}
	e.dims = len(warm[0])
	return e, nil
}

// Dims is the embedding dimensionality every Encode vector carries.
func (e *Engine) Dims() int { return e.dims }

// Close releases the resident WASM instance and its runtime.
func (e *Engine) Close(ctx context.Context) error {
	return e.runtime.Close(ctx)
}

// Encode embeds each text into an L2-normalized float32 vector of length Dims.
// Input is raw content — no query/document prefixes. A text with no
// in-vocabulary tokens embeds to the zero vector. Calls serialize on the shared
// instance.
func (e *Engine) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encodeBatch(ctx, texts)
}

// load decodes the three model blobs into the resident StaticModel via
// em_load_model. The host owns the input buffers and frees them here; a
// non-empty error payload from the module surfaces as a Go error.
func (e *Engine) load(ctx context.Context, b *modelBlobs) error {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()

	tokPtr, err := e.writeBlob(ctx, b.tokenizer)
	if err != nil {
		return err
	}
	defer e.free(ctx, tokPtr, uint32(len(b.tokenizer)))
	modelPtr, err := e.writeBlob(ctx, b.model)
	if err != nil {
		return err
	}
	defer e.free(ctx, modelPtr, uint32(len(b.model)))
	cfgPtr, err := e.writeBlob(ctx, b.config)
	if err != nil {
		return err
	}
	defer e.free(ctx, cfgPtr, uint32(len(b.config)))

	res, err := e.loadModel.Call(ctx,
		uint64(tokPtr), uint64(len(b.tokenizer)),
		uint64(modelPtr), uint64(len(b.model)),
		uint64(cfgPtr), uint64(len(b.config)),
	)
	if err != nil {
		return fmt.Errorf("em_load_model: %w", err)
	}
	errPtr, errLen := unpack(res[0])
	if errLen == 0 {
		return nil
	}
	msg, ok := e.module.Memory().Read(errPtr, errLen)
	if !ok {
		return fmt.Errorf("em_load_model failed and its error message at %d (%d bytes) is out of range", errPtr, errLen)
	}
	out := string(msg)
	e.free(ctx, errPtr, errLen)
	return fmt.Errorf("load model: %s", out)
}

// encodeBatch frames texts, runs one em_encode, and copies the flat matrix out.
// The caller holds e.mu (or is the single-threaded New).
func (e *Engine) encodeBatch(ctx context.Context, texts []string) ([][]float32, error) {
	frame := frameBatch(texts)
	inPtr, err := e.writeBlob(ctx, frame)
	if err != nil {
		return nil, err
	}
	defer e.free(ctx, inPtr, uint32(len(frame)))

	res, err := e.encode.Call(ctx, uint64(inPtr), uint64(len(frame)))
	if err != nil {
		return nil, fmt.Errorf("em_encode: %w", err)
	}
	outPtr, outLen := unpack(res[0])
	raw, ok := e.module.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("read encode output at %d (%d bytes) out of range", outPtr, outLen)
	}
	out := append([]byte(nil), raw...) // copy before freeing the WASM buffer
	e.free(ctx, outPtr, outLen)
	return parseMatrix(out, len(texts))
}

// writeBlob allocates a WASM buffer and copies data into it, returning its
// address. The caller frees it with free.
func (e *Engine) writeBlob(ctx context.Context, data []byte) (uint32, error) {
	res, err := e.alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("em_alloc(%d): %w", len(data), err)
	}
	ptr := uint32(res[0])
	if !e.module.Memory().Write(ptr, data) {
		return 0, fmt.Errorf("write %d bytes at %d out of range", len(data), ptr)
	}
	return ptr, nil
}

// free releases a WASM buffer previously handed out by em_alloc or packed out
// of em_load_model / em_encode. Best-effort: a dealloc failure never fails an
// otherwise-successful call.
func (e *Engine) free(ctx context.Context, ptr, n uint32) {
	_, _ = e.dealloc.Call(ctx, uint64(ptr), uint64(n))
}

// frameBatch encodes texts as [u32 count] then, per text, [u32 byte_len][utf8].
func frameBatch(texts []string) []byte {
	total := 4
	for _, t := range texts {
		total += 4 + len(t)
	}
	buf := make([]byte, 0, total)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(texts)))
	for _, t := range texts {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(t)))
		buf = append(buf, t...)
	}
	return buf
}

// parseMatrix decodes the [u32 rows][u32 dims][row-major f32] encode output and
// checks the row count against the request.
func parseMatrix(buf []byte, want int) ([][]float32, error) {
	if len(buf) < 8 {
		return nil, fmt.Errorf("encode output too short: %d bytes", len(buf))
	}
	rows := int(binary.LittleEndian.Uint32(buf[0:4]))
	dims := int(binary.LittleEndian.Uint32(buf[4:8]))
	if rows != want {
		return nil, fmt.Errorf("encode returned %d rows, want %d", rows, want)
	}
	need := 8 + rows*dims*4
	if len(buf) != need {
		return nil, fmt.Errorf("encode output is %d bytes, want %d (rows=%d dims=%d)", len(buf), need, rows, dims)
	}
	out := make([][]float32, rows)
	off := 8
	for r := range out {
		v := make([]float32, dims)
		for c := range v {
			v[c] = math.Float32frombits(binary.LittleEndian.Uint32(buf[off : off+4]))
			off += 4
		}
		out[r] = v
	}
	return out, nil
}

// unpack splits a (ptr << 32) | len return value into its pointer and length.
func unpack(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed)
}

// compileModule builds the compiler runtime over the shared on-disk AOT cache
// and compiles the embedded module.
func compileModule(ctx context.Context) (wazero.Runtime, wazero.CompiledModule, error) {
	ctx, cancel := context.WithTimeout(ctx, compileTimeout)
	defer cancel()

	dir, err := cache.Dir("wasm")
	if err != nil {
		return nil, nil, fmt.Errorf("resolve wasm cache dir: %w", err)
	}
	compilationCache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("open wasm compilation cache: %w", err)
	}

	rt, err := newCompilerRuntime(ctx, compilationCache)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := rt.CompileModule(ctx, wasmModule)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, nil, fmt.Errorf("compile embedcore.wasm: %w", err)
	}
	return rt, compiled, nil
}

// newCompilerRuntime builds the wazero compiler runtime. Unlike the format
// engine it omits WithCloseOnContextDone: this module is resident, so a per-call
// context finishing must not close the shared instance. NewRuntimeConfigCompiler
// panics on a host with no compiler backend rather than falling back to the
// (far slower) interpreter; the recover is scoped to exactly that construction.
func newCompilerRuntime(ctx context.Context, compilationCache wazero.CompilationCache) (rt wazero.Runtime, err error) {
	defer func() {
		if r := recover(); r != nil {
			rt, err = nil, fmt.Errorf(
				"wazero compiler backend unavailable on %s/%s: %v",
				runtime.GOOS, runtime.GOARCH, r,
			)
		}
	}()
	return wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(compilationCache).
		WithMemoryLimitPages(maxMemoryPages)), nil
}

// Cosine returns the cosine similarity of a and b: their dot product over the
// product of their L2 norms, in [-1, 1]. It panics on a length mismatch (a
// programming error — every Engine vector shares Dims). A zero-norm vector (a
// text with no in-vocabulary tokens) yields 0.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		panic(fmt.Sprintf("embed.Cosine: length mismatch %d != %d", len(a), len(b)))
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
