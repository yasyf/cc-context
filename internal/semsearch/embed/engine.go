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
	"errors"
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

// maxU32Bytes is the u32 linear-address ceiling that every frame count, text
// length, and total blob size must fit: the ABI passes each as a WASM u32, so a
// larger value would narrow silently and hand the guest a corrupt view. A var,
// not a const, so bounds tests can lower it to exercise the rejection path.
var maxU32Bytes uint64 = math.MaxUint32

// Engine is a resident model2vec inference engine. Construct it with New and
// release its WASM instance with Close. Encode is safe for concurrent use;
// calls serialize on a single shared instance.
type Engine struct {
	runtime   wazero.Runtime
	cache     wazero.CompilationCache
	compiled  wazero.CompiledModule
	module    api.Module
	alloc     api.Function
	dealloc   api.Function
	loadModel api.Function
	encode    api.Function

	mu   sync.Mutex
	dims int
}

// New resolves the pin's weights (downloading once into the cache), compiles the
// WASM module behind wazero's compiler backend, instantiates it resident, and
// loads the weights into its linear memory. It returns ErrWeightsUnavailable
// when the weights are neither cached nor downloadable.
func New(ctx context.Context, pin ModelPin) (*Engine, error) {
	blobs, err := resolveWeights(ctx, pin)
	if err != nil {
		return nil, err
	}

	rt, compilationCache, compiled, err := compileModule(ctx)
	if err != nil {
		return nil, err
	}

	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		_ = rt.Close(ctx)
		_ = compiled.Close(ctx)
		_ = compilationCache.Close(ctx)
		return nil, fmt.Errorf("instantiate embedcore.wasm: %w", err)
	}

	e := &Engine{
		runtime:   rt,
		cache:     compilationCache,
		compiled:  compiled,
		module:    mod,
		alloc:     mod.ExportedFunction("em_alloc"),
		dealloc:   mod.ExportedFunction("em_dealloc"),
		loadModel: mod.ExportedFunction("em_load_model"),
		encode:    mod.ExportedFunction("em_encode"),
	}

	if err := e.load(ctx, blobs); err != nil {
		_ = e.Close(ctx)
		return nil, err
	}

	// Warm up: JITs the encode path and captures the embedding dimensionality.
	warm, err := e.encodeBatch(ctx, []string{"warm up"})
	if err != nil {
		_ = e.Close(ctx)
		return nil, fmt.Errorf("warmup encode: %w", err)
	}
	e.dims = len(warm[0])
	return e, nil
}

// Dims is the embedding dimensionality every Encode vector carries.
func (e *Engine) Dims() int { return e.dims }

// Close releases the resident WASM instance, its compiled module, the runtime,
// and the on-disk compilation cache. wazero deliberately leaves a configured
// cache open when the runtime closes, so the cache — which retains the compiled
// module code — must be closed explicitly. Close nulls the retained handles so a
// closed Engine pins no model memory. It is idempotent and safe to call after a
// partial New.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var errs []error
	if e.runtime != nil {
		if err := e.runtime.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close runtime: %w", err))
		}
	}
	if e.compiled != nil {
		if err := e.compiled.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close compiled module: %w", err))
		}
	}
	if e.cache != nil {
		if err := e.cache.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close compilation cache: %w", err))
		}
	}
	e.runtime, e.compiled, e.cache, e.module = nil, nil, nil, nil
	e.alloc, e.dealloc, e.loadModel, e.encode = nil, nil, nil, nil
	return errors.Join(errs...)
}

// Encode embeds each text into an L2-normalized float32 vector of length Dims.
// Input is raw content — no query/document prefixes. A text with no
// in-vocabulary tokens embeds to the zero vector. Calls serialize on the shared
// instance.
func (e *Engine) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// An in-flight WASM encode runs to completion; local calls stay under 15 ms.
	return e.encodeBatch(ctx, texts)
}

// load decodes the three model blobs into the resident StaticModel via
// em_load_model. The host owns the input buffers and frees them here; a
// non-empty error payload from the module surfaces as a Go error.
func (e *Engine) load(ctx context.Context, b *modelBlobs) error {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()

	tokPtr, err := writeBlob(ctx, e, b.tokenizer)
	if err != nil {
		return err
	}
	defer e.freeBlob(ctx, tokPtr, b.tokenizer)
	modelPtr, err := writeBlob(ctx, e, b.model)
	if err != nil {
		return err
	}
	defer e.freeBlob(ctx, modelPtr, b.model)
	cfgPtr, err := writeBlob(ctx, e, b.config)
	if err != nil {
		return err
	}
	defer e.freeBlob(ctx, cfgPtr, b.config)

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
	frame, err := frameBatch(texts)
	if err != nil {
		return nil, err
	}
	inPtr, err := writeBlob(ctx, e, frame)
	if err != nil {
		return nil, err
	}
	defer e.freeBlob(ctx, inPtr, frame)

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

// blobWriter is the guest-memory boundary writeBlob drives: reserve n bytes,
// copy a payload into the reservation, and release a reservation. The Engine
// binds it to its em_alloc/em_dealloc exports and linear memory; tests fake it
// to exercise the write-failure path without a multi-GiB allocation.
type blobWriter interface {
	guestAlloc(ctx context.Context, n uint32) (uint32, error)
	guestWrite(ptr uint32, data []byte) bool
	guestFree(ctx context.Context, ptr, n uint32)
}

func (e *Engine) guestAlloc(ctx context.Context, n uint32) (uint32, error) {
	res, err := e.alloc.Call(ctx, uint64(n))
	if err != nil {
		return 0, err
	}
	return uint32(res[0]), nil //nolint:gosec // wasm32: em_alloc returns a 32-bit pointer in the low half
}

func (e *Engine) guestWrite(ptr uint32, data []byte) bool {
	return e.module.Memory().Write(ptr, data)
}

func (e *Engine) guestFree(ctx context.Context, ptr, n uint32) {
	e.free(ctx, ptr, n)
}

// writeBlob reserves a WASM buffer and copies data into it, returning its
// address. It rejects a blob larger than the u32 linear-address space before
// reserving, and releases the reservation if the copy fails, so a failed write
// leaks nothing. The caller frees a successful reservation with free.
func writeBlob(ctx context.Context, w blobWriter, data []byte) (uint32, error) {
	if uint64(len(data)) > maxU32Bytes {
		return 0, fmt.Errorf("blob of %d bytes exceeds the u32 linear-address limit", len(data))
	}
	ptr, err := w.guestAlloc(ctx, uint32(len(data))) //nolint:gosec // guarded by the u32 check above
	if err != nil {
		return 0, fmt.Errorf("em_alloc(%d): %w", len(data), err)
	}
	if !w.guestWrite(ptr, data) {
		w.guestFree(ctx, ptr, uint32(len(data))) //nolint:gosec // guarded by the u32 check above
		return 0, fmt.Errorf("write %d bytes at %d out of range", len(data), ptr)
	}
	return ptr, nil
}

// freeBlob releases a writeBlob allocation.
func (e *Engine) freeBlob(ctx context.Context, ptr uint32, data []byte) {
	e.free(ctx, ptr, uint32(len(data))) //nolint:gosec // wasm32: blob sizes fit 32-bit linear memory
}

// free releases a WASM buffer previously handed out by em_alloc or packed out
// of em_load_model / em_encode. Best-effort: a dealloc failure never fails an
// otherwise-successful call.
func (e *Engine) free(ctx context.Context, ptr, n uint32) {
	_, _ = e.dealloc.Call(ctx, uint64(ptr), uint64(n))
}

// frameBatch encodes texts as [u32 count] then, per text, [u32 byte_len][utf8].
// It errors when the batch count, any text length, or the total frame size
// exceeds what the u32 ABI can address, rather than narrowing silently.
func frameBatch(texts []string) ([]byte, error) {
	if uint64(len(texts)) > maxU32Bytes {
		return nil, fmt.Errorf("batch of %d texts exceeds the u32 frame-count limit", len(texts))
	}
	total := uint64(4)
	for _, t := range texts {
		if uint64(len(t)) > maxU32Bytes {
			return nil, fmt.Errorf("text of %d bytes exceeds the u32 frame-length limit", len(t))
		}
		total += 4 + uint64(len(t))
	}
	if total > maxU32Bytes {
		return nil, fmt.Errorf("framed batch of %d bytes exceeds the u32 address limit", total)
	}
	buf := make([]byte, 0, total)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(texts))) //nolint:gosec // guarded by the count check above
	for _, t := range texts {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(t))) //nolint:gosec // guarded by the per-text length check above
		buf = append(buf, t...)
	}
	return buf, nil
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
	return uint32(packed >> 32), uint32(packed) //nolint:gosec // wasm32: em_encode packs ptr and len into one uint64
}

// compileModule builds the compiler runtime over the shared on-disk AOT cache
// and compiles the embedded module. The caller owns the returned cache and
// closes it (via Engine.Close); wazero will not close a configured cache itself.
func compileModule(ctx context.Context) (wazero.Runtime, wazero.CompilationCache, wazero.CompiledModule, error) {
	ctx, cancel := context.WithTimeout(ctx, compileTimeout)
	defer cancel()

	dir, err := cache.Dir("wasm")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve wasm cache dir: %w", err)
	}
	compilationCache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open wasm compilation cache: %w", err)
	}

	rt, err := newCompilerRuntime(ctx, compilationCache)
	if err != nil {
		_ = compilationCache.Close(ctx)
		return nil, nil, nil, err
	}
	compiled, err := rt.CompileModule(ctx, wasmModule)
	if err != nil {
		_ = rt.Close(ctx)
		_ = compilationCache.Close(ctx)
		return nil, nil, nil, fmt.Errorf("compile embedcore.wasm: %w", err)
	}
	return rt, compilationCache, compiled, nil
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
