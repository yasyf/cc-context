package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/semsearch"
)

// schemaVersion is the on-disk index-cache format version. A mismatch discards
// the cache and rebuilds — bump it whenever the persisted layout changes.
const schemaVersion = 1

// Cache file names within a repo's cache dir.
const (
	manifestFile = "manifest.json"
	chunksFile   = "chunks.json"
	vectorsFile  = "vectors.bin"
)

// fileManifest records one file's modification time and its chunk range within
// the flat chunk/vector arrays — semble's FileManifestEntry.
type fileManifest struct {
	Path    string `json:"path"`
	MtimeNs int64  `json:"mtime_ns"`
	Start   int    `json:"start"`
	Count   int    `json:"count"`
}

// manifest is the cache header plus the per-file chunk ranges, in walk order.
type manifest struct {
	Schema  int            `json:"schema"`
	Model   string         `json:"model"`
	Content string         `json:"content"`
	Chunker string         `json:"chunker"`
	Dims    int            `json:"dims"`
	Files   []fileManifest `json:"files"`
}

// persisted is a loaded, self-consistent cache: the manifest, the flat chunk
// list, and the parallel vector matrix.
type persisted struct {
	manifest manifest
	chunks   []semsearch.Chunk
	vectors  [][]float32
}

// cacheDir resolves the per-repo cache directory, keyed by the sha256 of the
// resolved absolute repo path under cache.Dir("semsearch") — semble's
// find_index_from_cache_folder scheme.
func cacheDir(root string) (string, error) {
	sum := sha256.Sum256([]byte(root))
	return cache.Dir("semsearch", hex.EncodeToString(sum[:]))
}

// resolveRoot returns root as an absolute, symlink-resolved path used both as
// the cache key and the walk root.
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", root, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// loadPersisted loads and validates a repo's cache, returning nil (no error)
// when it is absent, malformed, or incompatible with the requested parameters —
// mirroring semble's load_previous_for_incremental (a bad cache is rebuilt, not
// fatal).
func loadPersisted(dir, model, content, chunker string) *persisted {
	man, err := readManifest(dir)
	if err != nil {
		return nil
	}
	if man.Schema != schemaVersion || man.Model != model || man.Content != content || man.Chunker != chunker {
		return nil
	}
	chunks, err := readChunks(dir)
	if err != nil {
		return nil
	}
	vectors, err := readVectors(dir)
	if err != nil {
		return nil
	}
	if len(chunks) != len(vectors) || (man.Dims != 0 && len(vectors) > 0 && len(vectors[0]) != man.Dims) {
		return nil
	}
	// Every file's chunk range must line up with the flat arrays.
	next := 0
	for _, f := range man.Files {
		if f.Start != next || f.Start+f.Count > len(chunks) {
			return nil
		}
		next += f.Count
	}
	if next != len(chunks) {
		return nil
	}
	return &persisted{manifest: man, chunks: chunks, vectors: vectors}
}

// entryByPath indexes a persisted manifest's file entries by repo-relative path.
func (p *persisted) entryByPath() map[string]fileManifest {
	m := make(map[string]fileManifest, len(p.manifest.Files))
	for _, f := range p.manifest.Files {
		m[f.Path] = f
	}
	return m
}

// store writes the manifest, chunks, and vector matrix atomically into dir.
func store(dir string, man manifest, chunks []semsearch.Chunk, vectors [][]float32) error {
	manData, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	chunkData, err := json.Marshal(chunks)
	if err != nil {
		return fmt.Errorf("marshal chunks: %w", err)
	}
	if err := cache.Store(filepath.Join(dir, chunksFile), chunkData, 0o640); err != nil {
		return err
	}
	if err := cache.Store(filepath.Join(dir, vectorsFile), encodeVectors(vectors), 0o640); err != nil {
		return err
	}
	// Manifest last: it is the validity gate, so it must not name chunks/vectors
	// that are not yet on disk.
	return cache.Store(filepath.Join(dir, manifestFile), manData, 0o640)
}

func readManifest(dir string) (manifest, error) {
	var man manifest
	data, err := os.ReadFile(filepath.Join(dir, manifestFile)) //nolint:gosec // dir derives from the repo-path sha256, not user input
	if err != nil {
		return man, err
	}
	if err := json.Unmarshal(data, &man); err != nil {
		return man, fmt.Errorf("decode manifest: %w", err)
	}
	return man, nil
}

func readChunks(dir string) ([]semsearch.Chunk, error) {
	data, err := os.ReadFile(filepath.Join(dir, chunksFile)) //nolint:gosec // dir derives from the repo-path sha256, not user input
	if err != nil {
		return nil, err
	}
	var chunks []semsearch.Chunk
	if err := json.Unmarshal(data, &chunks); err != nil {
		return nil, fmt.Errorf("decode chunks: %w", err)
	}
	return chunks, nil
}

// encodeVectors frames a vector matrix as [u32 rows][u32 dims][row-major f32],
// little-endian — the same framing the WASM engine emits.
func encodeVectors(vectors [][]float32) []byte {
	dims := 0
	if len(vectors) > 0 {
		dims = len(vectors[0])
	}
	buf := make([]byte, 0, 8+len(vectors)*dims*4)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(vectors))) //nolint:gosec // matrix dims fit the u32 framing
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dims))
	for _, row := range vectors {
		for _, v := range row {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
		}
	}
	return buf
}

func readVectors(dir string) ([][]float32, error) {
	data, err := os.ReadFile(filepath.Join(dir, vectorsFile)) //nolint:gosec // dir derives from the repo-path sha256, not user input
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, errors.New("vectors.bin too short")
	}
	rows := int(binary.LittleEndian.Uint32(data[0:4]))
	dims := int(binary.LittleEndian.Uint32(data[4:8]))
	if len(data) != 8+rows*dims*4 {
		return nil, fmt.Errorf("vectors.bin is %d bytes, want %d (rows=%d dims=%d)", len(data), 8+rows*dims*4, rows, dims)
	}
	out := make([][]float32, rows)
	off := 8
	for i := range out {
		row := make([]float32, dims)
		for j := range row {
			row[j] = math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
			off += 4
		}
		out[i] = row
	}
	return out, nil
}
