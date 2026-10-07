package index

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/gitdir"
	"github.com/yasyf/cc-context/internal/semsearch"
)

// schemaVersion is the on-disk index-cache format version. A mismatch discards
// the cache and rebuilds — bump it whenever the persisted layout changes.
const schemaVersion = 3

// Cache file names within a repo's cache dir.
const (
	manifestFile = "manifest.json"
	chunksFile   = "chunks.json"
	vectorsFile  = "vectors.bin"
	lastUsedFile = "last_used"
	rootFile     = "root"
	seedsDir     = "seeds"
)

const maxSeedCandidates = 8

// fileManifest records one file's modification time, content hash, and chunk
// range within the flat chunk/vector arrays — semble's FileManifestEntry.
type fileManifest struct {
	Path    string `json:"path"`
	MtimeNs int64  `json:"mtime_ns"`
	Hash    string `json:"sha256"`
	Start   int    `json:"start"`
	Count   int    `json:"count"`
}

// manifest is the cache header plus the per-file chunk ranges, in walk order.
type manifest struct {
	Schema     int            `json:"schema"`
	Generation string         `json:"generation"`
	Model      string         `json:"model"`
	Content    string         `json:"content"`
	Chunker    string         `json:"chunker"`
	Dims       int            `json:"dims"`
	Files      []fileManifest `json:"files"`
}

type chunkEnvelope struct {
	Generation string            `json:"generation"`
	Chunks     []semsearch.Chunk `json:"chunks"`
}

// persisted is a loaded, self-consistent cache: the manifest, the flat chunk
// list, and the parallel vector matrix.
type persisted struct {
	manifest manifest
	chunks   []semsearch.Chunk
	vectors  [][]float32
}

// cacheDir resolves the per-repo cache directory, keyed by the sha256 of the
// resolved absolute repo path under cache.Dir(ctx, "semsearch") — semble's
// find_index_from_cache_folder scheme.
func cacheDir(ctx context.Context, root string) (string, error) {
	sum := sha256.Sum256([]byte(root))
	return cache.Dir(ctx, "semsearch", hex.EncodeToString(sum[:]))
}

func variantKey(model, content, chunker string, dims int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d %q %q %q %d", schemaVersion, model, content, chunker, dims))
	return hex.EncodeToString(sum[:])
}

func variantCacheDir(ctx context.Context, root, vKey string) (string, error) {
	repoKey := sha256.Sum256([]byte(root))
	return cache.Dir(ctx, "semsearch", hex.EncodeToString(repoKey[:]), vKey)
}

// familyDir resolves the directory holding one pointer per worktree that shares
// root's git common dir and path within it, or "" outside a git working tree.
func familyDir(ctx context.Context, root, vKey string) (string, error) {
	gitRoot := gitdir.Root(root)
	if gitRoot == "" {
		return "", nil
	}
	common := gitdir.CommonDir(gitRoot)
	if common == "" {
		return "", nil
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	rel, err := filepath.Rel(gitRoot, root)
	if err != nil {
		return "", fmt.Errorf("relativize %q to %q: %w", root, gitRoot, err)
	}
	sum := sha256.Sum256([]byte(common + "\x00" + filepath.ToSlash(rel)))
	return cache.Dir(ctx, "semsearch", seedsDir, hex.EncodeToString(sum[:]), vKey)
}

// siblingIndexes lists the most recently stored indexes a family's pointers name,
// other than self.
func siblingIndexes(famDir, self string) []string {
	entries, err := os.ReadDir(famDir)
	if err != nil {
		return nil
	}
	type candidate struct {
		dir    string
		stored time.Time
	}
	var found []candidate
	for _, e := range entries {
		if !isKey(e.Name()) {
			continue
		}
		dir := readPointer(filepath.Join(famDir, e.Name()))
		fi, err := os.Stat(filepath.Join(dir, manifestFile))
		if dir == "" || dir == self || err != nil {
			continue
		}
		found = append(found, candidate{dir, fi.ModTime()})
	}
	slices.SortFunc(found, func(a, b candidate) int { return b.stored.Compare(a.stored) })
	dirs := make([]string, 0, min(len(found), maxSeedCandidates))
	for _, c := range found[:min(len(found), maxSeedCandidates)] {
		dirs = append(dirs, c.dir)
	}
	return dirs
}

func readPointer(pointer string) string {
	data, err := os.ReadFile(pointer) //nolint:gosec // pointer lives under the trusted cache dir
	if err != nil {
		return ""
	}
	return string(data)
}

func hasManifest(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, manifestFile))
	return err == nil
}

func recordRoot(dir, root string) error {
	return cache.Store(filepath.Join(filepath.Dir(dir), rootFile), []byte(root), 0o640)
}

func touchLastUsed(dir string) error {
	path := filepath.Join(dir, lastUsedFile)
	now := time.Now()
	if err := os.Chtimes(path, now, now); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, nil, 0o640) //nolint:gosec // path is under the trusted cache dir
}

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// CacheDir resolves the persistent index-cache directory for root.
func CacheDir(ctx context.Context, root string) (string, error) {
	root, err := ResolveRoot(root)
	if err != nil {
		return "", err
	}
	return cacheDir(ctx, root)
}

// ResolveRoot returns root as an absolute, symlink-resolved path used both as
// the cache key and the walk root.
func ResolveRoot(root string) (string, error) {
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
// fatal). dims is the live embedder's output width: a cache whose vectors do not
// match it is rejected (rebuilt) so mismatched vectors never reach rank.Cosine.
func loadPersisted(dir, model, content, chunker string, dims int) *persisted {
	man, err := readManifest(dir)
	if err != nil {
		return nil
	}
	if man.Schema != schemaVersion || man.Model != model || man.Content != content || man.Chunker != chunker {
		return nil
	}
	chunkData, err := readChunks(dir)
	if err != nil {
		return nil
	}
	vectorGeneration, vectors, err := readVectors(dir)
	if err != nil {
		return nil
	}
	if man.Generation == "" || man.Generation != chunkData.Generation || man.Generation != vectorGeneration {
		return nil
	}
	chunks := chunkData.Chunks
	if len(chunks) != len(vectors) || (man.Dims != 0 && len(vectors) > 0 && len(vectors[0]) != man.Dims) {
		return nil
	}
	if dims != 0 && ((man.Dims != 0 && man.Dims != dims) || (len(vectors) > 0 && len(vectors[0]) != dims)) {
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

// store writes the manifest, chunks, and vector matrix into dir.
func store(dir string, man manifest, chunks []semsearch.Chunk, vectors [][]float32) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create cache dir %q: %w", dir, err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate cache generation: %w", err)
	}
	man.Generation = hex.EncodeToString(nonce[:])

	manData, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	chunkData, err := json.Marshal(chunkEnvelope{
		Generation: man.Generation,
		Chunks:     chunks,
	})
	if err != nil {
		return fmt.Errorf("marshal chunks: %w", err)
	}
	if err := cache.Store(filepath.Join(dir, chunksFile), chunkData, 0o640); err != nil {
		return err
	}
	if err := cache.Store(filepath.Join(dir, vectorsFile), encodeVectors(man.Generation, vectors), 0o640); err != nil {
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

func readChunks(dir string) (chunkEnvelope, error) {
	data, err := os.ReadFile(filepath.Join(dir, chunksFile)) //nolint:gosec // dir derives from the repo-path sha256, not user input
	if err != nil {
		return chunkEnvelope{}, err
	}
	var envelope chunkEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return chunkEnvelope{}, fmt.Errorf("decode chunks: %w", err)
	}
	return envelope, nil
}

// encodeVectors frames a vector matrix as
// [u32 generation length][generation][u32 rows][u32 dims][row-major f32].
func encodeVectors(generation string, vectors [][]float32) []byte {
	dims := 0
	if len(vectors) > 0 {
		dims = len(vectors[0])
	}
	buf := make([]byte, 0, 12+len(generation)+len(vectors)*dims*4)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(generation))) //nolint:gosec // generated nonce length fits the u32 framing
	buf = append(buf, generation...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(vectors))) //nolint:gosec // matrix dims fit the u32 framing
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dims))
	for _, row := range vectors {
		for _, v := range row {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
		}
	}
	return buf
}

func readVectors(dir string) (string, [][]float32, error) {
	data, err := os.ReadFile(filepath.Join(dir, vectorsFile)) //nolint:gosec // dir derives from the repo-path sha256, not user input
	if err != nil {
		return "", nil, err
	}
	if len(data) < 12 {
		return "", nil, errors.New("vectors.bin too short")
	}
	generationLen := binary.LittleEndian.Uint32(data[0:4])
	if uint64(generationLen)+12 > uint64(len(data)) {
		return "", nil, fmt.Errorf("vectors.bin generation length %d exceeds %d-byte payload", generationLen, len(data))
	}
	generationEnd := 4 + int(generationLen)
	generation := string(data[4:generationEnd])
	rowsValue := binary.LittleEndian.Uint32(data[generationEnd : generationEnd+4])
	dimsValue := binary.LittleEndian.Uint32(data[generationEnd+4 : generationEnd+8])
	want := uint64(generationEnd+8) + uint64(rowsValue)*uint64(dimsValue)*4
	if uint64(len(data)) != want {
		return "", nil, fmt.Errorf("vectors.bin is %d bytes, want %d (rows=%d dims=%d)", len(data), want, rowsValue, dimsValue)
	}
	rows := int(rowsValue)
	dims := int(dimsValue)
	// One backing array sliced per row, not one allocation per row: a monorepo's
	// ~144k rows otherwise become that many separately-scanned 1 KiB heap objects.
	flat := make([]float32, rows*dims)
	off := generationEnd + 8
	for i := range flat {
		flat[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
		off += 4
	}
	out := make([][]float32, rows)
	for i := range out {
		out[i] = flat[i*dims : (i+1)*dims : (i+1)*dims]
	}
	return generation, out, nil
}
