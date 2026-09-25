package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

// embedBatchSize bounds one Encode call so a large repo does not frame its whole
// chunk corpus into WASM memory at once. model2vec is batch-invariant, so the
// split does not change any vector.
const embedBatchSize = 512

// maxChunkWorkers caps the parallel chunking fan-out. Embedding downstream
// serializes on one shared WASM instance, so chunking wider than a handful of cores
// buys little — while one MCP server per session taking runtime.NumCPU() each
// oversubscribes the machine by an order of magnitude.
const maxChunkWorkers = 8

// Embedder embeds raw text into fixed-width L2-normalized vectors — the resident
// model2vec engine (internal/semsearch/embed) satisfies it.
type Embedder interface {
	Encode(ctx context.Context, texts []string) ([][]float32, error)
	Dims() int
}

// Index is a repo's loaded chunk corpus and its parallel embedding matrix:
// Vectors[i] embeds Chunks[i]. It is the input to the ranking stage. BM25 is the
// prebuilt corpus BM25 over Chunks in the same order — built eagerly on Load,
// never persisted — so a warm reuse serves queries without rebuilding it.
type Index struct {
	Root       string
	Chunks     []semsearch.Chunk
	Vectors    [][]float32
	BM25       *rank.BM25
	Reindexed  int // files re-embedded this Load; 0 means a fully warm cache hit
	TotalFiles int
}

// Load loads the repo's cached index or builds it, reusing unchanged files'
// chunks and vectors and re-embedding only changed ones — a port of semble's
// create_index_from_path incremental path. It fails loudly (never falls back to
// BM25-only) when the embedder is unavailable, and errors when the repo has no
// indexable file. The build holds a per-repo lock so concurrent callers do not
// race the cache.
func Load(ctx context.Context, emb Embedder, root string, content []ContentType, chunker Chunker, modelID string) (*Index, error) {
	root, err := ResolveRoot(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat repo %q: %w", root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("semsearch: %q is not a directory", root)
	}

	contentK := ContentKey(content)
	chunkerID := chunker.ID()
	dir, err := variantCacheDir(root, modelID, contentK, chunkerID, emb.Dims())
	if err != nil {
		return nil, err
	}
	exts := Extensions(content)

	var idx *Index
	err = cache.WithLock(ctx, dir, "index", func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		prev := loadPersisted(dir, modelID, contentK, chunkerID, emb.Dims())
		built, berr := build(ctx, emb, root, exts, chunker, prev)
		if berr != nil {
			return berr
		}
		man := manifest{
			Schema:  schemaVersion,
			Model:   modelID,
			Content: contentK,
			Chunker: chunkerID,
			Dims:    emb.Dims(),
			Files:   built.files,
		}
		if !unchanged(built, prev) {
			if err := store(dir, man, built.chunks, built.vectors); err != nil {
				return err
			}
		}
		idx = &Index{
			Root:       root,
			Chunks:     built.chunks,
			Vectors:    built.vectors,
			BM25:       rank.BuildBM25(built.chunks),
			Reindexed:  built.reindexed,
			TotalFiles: len(built.files),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A build's peak and a warm load's decode buffers both dwarf the index left
	// behind; Go's scavenger would return those pages only over many minutes.
	debug.FreeOSMemory()
	return idx, nil
}

// unchanged reports whether built reproduces prev exactly, so rewriting the cache
// would only burn a whole-corpus marshal and a 200 MB+ rewrite to install identical
// bytes. Every file reused prev's chunks at a matching mtime, so equal file counts
// mean equal sets: a deletion shortens the list and an addition re-embeds.
func unchanged(built *buildResult, prev *persisted) bool {
	return prev != nil && built.reindexed == 0 && len(built.files) == len(prev.manifest.Files)
}

// buildResult is the assembled corpus plus its manifest.
type buildResult struct {
	chunks    []semsearch.Chunk
	vectors   [][]float32
	files     []fileManifest
	reindexed int
}

// fileResult is one file's chunking outcome, filled concurrently and assembled
// in walk order.
type fileResult struct {
	rel    string
	mtime  int64
	valid  bool
	reuse  bool
	prev   fileManifest
	chunks []semsearch.Chunk
}

// build walks the repo, chunks changed files in parallel (reusing unchanged
// ones from prev), then embeds every new chunk in one serialized pass.
func build(ctx context.Context, emb Embedder, root string, exts []string, chunker Chunker, prev *persisted) (*buildResult, error) {
	paths, err := WalkFiles(ctx, root, exts)
	if err != nil {
		return nil, err
	}
	var prevEntries map[string]fileManifest
	if prev != nil {
		prevEntries = prev.entryByPath()
	}

	results := make([]fileResult, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(min(runtime.NumCPU(), maxChunkWorkers))
	for i, abs := range paths {
		if gctx.Err() != nil {
			break
		}
		i, abs := i, abs
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			results[i] = chunkFile(abs, root, chunker, prevEntries)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	res := &buildResult{}
	var toEmbed []string
	var toEmbedSlots []int
	for _, r := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !r.valid {
			continue
		}
		start := len(res.chunks)
		if r.reuse {
			res.chunks = append(res.chunks, prev.chunks[r.prev.Start:r.prev.Start+r.prev.Count]...)
			res.vectors = append(res.vectors, prev.vectors[r.prev.Start:r.prev.Start+r.prev.Count]...)
			res.files = append(res.files, fileManifest{Path: r.rel, MtimeNs: r.mtime, Start: start, Count: r.prev.Count})
			continue
		}
		for _, c := range r.chunks {
			toEmbedSlots = append(toEmbedSlots, len(res.vectors))
			res.vectors = append(res.vectors, nil)
			res.chunks = append(res.chunks, c)
			toEmbed = append(toEmbed, c.Content)
		}
		res.files = append(res.files, fileManifest{Path: r.rel, MtimeNs: r.mtime, Start: start, Count: len(r.chunks)})
		res.reindexed++
	}

	if len(res.chunks) == 0 {
		return nil, fmt.Errorf("semsearch: no indexable files under %s", root)
	}
	results = nil
	if prev != nil {
		prev.chunks, prev.vectors = nil, nil
	}

	if len(toEmbed) > 0 {
		embs, err := encodeAll(ctx, emb, toEmbed)
		if err != nil {
			return nil, err
		}
		for i, slot := range toEmbedSlots {
			res.vectors[slot] = embs[i]
		}
	}
	return res, nil
}

// chunkFile classifies one file and, when it is not a warm cache hit, reads and
// chunks it. A read/stat error or a non-valid status marks the file skipped,
// mirroring semble's suppress(OSError).
func chunkFile(abs, root string, chunker Chunker, prevEntries map[string]fileManifest) fileResult {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return fileResult{}
	}
	rel = filepath.ToSlash(rel)
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileBytes {
		return fileResult{}
	}
	mtime := fi.ModTime().UnixNano()

	if e, ok := prevEntries[rel]; ok && e.MtimeNs == mtime {
		return fileResult{rel: rel, mtime: mtime, valid: true, reuse: true, prev: e}
	}

	text, err := readFileText(abs)
	if err != nil || (fi.Size() < emptyFileBytes && strings.TrimSpace(text) == "") {
		return fileResult{}
	}
	return fileResult{
		rel:    rel,
		mtime:  mtime,
		valid:  true,
		chunks: chunker.ChunkFile(rel, DetectLanguage(rel), text),
	}
}

// encodeAll embeds texts in bounded batches on the serialized engine.
func encodeAll(ctx context.Context, emb Embedder, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += embedBatchSize {
		end := min(i+embedBatchSize, len(texts))
		vecs, err := emb.Encode(ctx, texts[i:end])
		if err != nil {
			return nil, fmt.Errorf("embed chunks: %w", err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}
