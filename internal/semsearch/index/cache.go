package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/gitdir"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semsearch"
)

// schemaVersion is the on-disk index-cache format version. A mismatch discards
// the cache and rebuilds — bump it whenever the persisted layout changes.
// mappedSchemaVersion (one worktree-private set of mapped files) and
// legacySchemaVersion (JSON chunks) are the formats before it, still read so an
// existing index converts without re-embedding.
const (
	schemaVersion       = 5
	mappedSchemaVersion = 4
	legacySchemaVersion = 3
)

var priorSchemaVersions = []int{mappedSchemaVersion, legacySchemaVersion}

// Cache file names within a repo's cache dir.
const (
	manifestFile     = "manifest.json"
	chunksFile       = "chunks.bin"
	legacyChunksFile = "chunks.json"
	vectorsFile      = "vectors.bin"
	termsFile        = "terms.bin"
	lastUsedFile     = "last_used"
	rootFile         = "root"
	seedsDir         = "seeds"
)

const maxSeedCandidates = 8

// fileManifest records one file's modification time, content hash, and chunk
// range within the flat chunk/vector arrays — semble's FileManifestEntry — plus,
// from schema 5, the segment holding those chunks and their offset within it.
type fileManifest struct {
	Path    string `json:"path"`
	MtimeNs int64  `json:"mtime_ns"`
	Hash    string `json:"sha256"`
	Start   int    `json:"start"`
	Count   int    `json:"count"`
	Segment int    `json:"segment,omitempty"`
	Offset  int    `json:"offset,omitempty"`
}

// manifest is the cache header plus the per-file chunk ranges, in walk order.
type manifest struct {
	Schema     int            `json:"schema"`
	Generation string         `json:"generation"`
	Model      string         `json:"model"`
	Content    string         `json:"content"`
	Chunker    string         `json:"chunker"`
	Dims       int            `json:"dims"`
	Segments   []string       `json:"segments,omitempty"`
	Files      []fileManifest `json:"files"`
}

type chunkEnvelope struct {
	Generation string            `json:"generation"`
	Chunks     []semsearch.Chunk `json:"chunks"`
}

// persisted is a loaded, self-consistent cache: the manifest, the flat chunk
// list, the parallel vector matrix, and the corpus BM25 postings. The chunks,
// vectors, and terms alias files, which a legacy cache maps only for its
// vectors and which has no terms; whoever drops a persisted unmaps its files.
type persisted struct {
	manifest manifest
	chunks   []semsearch.Chunk
	vectors  [][]float32
	terms    *corpusTerms
	files    *mappedFiles
}

// cacheEnv names a directory that replaces cache.Dir(ctx, "semsearch") as the
// index cache, so the indexes can live on a volume of their own and every
// process naming it shares one cache whatever its $CLAUDE_PLUGIN_DATA.
const cacheEnv = "CCX_SEMSEARCH_CACHE"

// semsearchDir resolves (and creates) the index-cache directory joined from sub.
func semsearchDir(ctx context.Context, sub ...string) (string, error) {
	root := render.Getenv(ctx, cacheEnv)
	if root == "" {
		return cache.Dir(ctx, append([]string{"semsearch"}, sub...)...)
	}
	dir := filepath.Join(append([]string{root}, sub...)...)
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // rooted at the operator's CCX_SEMSEARCH_CACHE, not user free-text
		return "", fmt.Errorf("create cache dir %q: %w", dir, err)
	}
	return dir, nil
}

// movedIndexes lists the variant dirs root's index occupied under every
// cache.Dir root before cacheEnv relocated the cache, so a first Load converts
// one instead of re-embedding the repo.
func movedIndexes(ctx context.Context, rootKey string, vKeys ...string) []string {
	if render.Getenv(ctx, cacheEnv) == "" {
		return nil
	}
	var dirs []string
	for _, base := range cache.Roots(ctx) {
		for _, vKey := range vKeys {
			dirs = append(dirs, filepath.Join(base, "semsearch", rootKey, vKey))
		}
	}
	return dirs
}

// cacheDir resolves the per-repo cache directory, keyed by the sha256 of the
// resolved absolute repo path — semble's find_index_from_cache_folder scheme.
func cacheDir(ctx context.Context, root string) (string, error) {
	return semsearchDir(ctx, rootKey(root))
}

func rootKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}

func variantKey(schema int, model, content, chunker string, dims int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d %q %q %q %d", schema, model, content, chunker, dims))
	return hex.EncodeToString(sum[:])
}

func variantCacheDir(ctx context.Context, root, vKey string) (string, error) {
	return semsearchDir(ctx, rootKey(root), vKey)
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
	return semsearchDir(ctx, seedsDir, hex.EncodeToString(sum[:]), vKey)
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
// A legacy-schema cache loads its chunks onto the heap and carries no terms.
func loadPersisted(dir, model, content, chunker string, dims int) *persisted {
	man, err := readManifest(dir)
	if err != nil {
		return nil
	}
	if man.Model != model || man.Content != content || man.Chunker != chunker {
		return nil
	}
	var p *persisted
	switch man.Schema {
	case schemaVersion:
		p = loadLayered(dir, man)
	case mappedSchemaVersion:
		p = loadMapped(dir, man)
	case legacySchemaVersion:
		p = loadLegacy(dir, man)
	}
	if p == nil {
		return nil
	}
	if !consistent(p, dims) {
		p.files.unmap()
		return nil
	}
	return p
}

func consistent(p *persisted, dims int) bool {
	man, chunks, vectors := p.manifest, p.chunks, p.vectors
	if len(chunks) != len(vectors) || (man.Dims != 0 && len(vectors) > 0 && len(vectors[0]) != man.Dims) {
		return false
	}
	if dims != 0 && ((man.Dims != 0 && man.Dims != dims) || (len(vectors) > 0 && len(vectors[0]) != dims)) {
		return false
	}
	if p.terms != nil && len(p.terms.lengths) != len(chunks) {
		return false
	}
	// Every file's chunk range must line up with the flat arrays.
	next := 0
	for _, f := range man.Files {
		if f.Start != next || f.Start+f.Count > len(chunks) {
			return false
		}
		next += f.Count
	}
	return next == len(chunks)
}

func loadMapped(dir string, man manifest) *persisted {
	var maps []*mapping
	for _, name := range []string{vectorsFile, chunksFile} {
		m, err := mapFile(filepath.Join(dir, name))
		if err != nil {
			unmapAll(maps)
			return nil
		}
		maps = append(maps, m)
	}
	vectorGeneration, vectors, verr := readVectorsMapped(maps[0].data)
	chunkGeneration, chunks, cerr := readChunksMapped(maps[1].data, man.Files)
	if verr != nil || cerr != nil || man.Generation == "" || man.Generation != vectorGeneration || man.Generation != chunkGeneration {
		unmapAll(maps)
		return nil
	}
	return &persisted{manifest: man, chunks: chunks, vectors: vectors, files: &mappedFiles{maps: maps}}
}

func loadLegacy(dir string, man manifest) *persisted {
	chunkData, err := readChunks(dir)
	if err != nil {
		return nil
	}
	vm, err := mapFile(filepath.Join(dir, vectorsFile))
	if err != nil {
		return nil
	}
	vectorGeneration, vectors, err := readVectorsMapped(vm.data)
	if err != nil || man.Generation == "" || man.Generation != chunkData.Generation || man.Generation != vectorGeneration {
		unmapAll([]*mapping{vm})
		return nil
	}
	return &persisted{manifest: man, chunks: chunkData.Chunks, vectors: vectors, files: &mappedFiles{maps: []*mapping{vm}}}
}

func (p *persisted) unmap() {
	if p != nil {
		p.files.unmap()
	}
}

// entryByPath indexes a persisted manifest's file entries by repo-relative path.
func (p *persisted) entryByPath() map[string]fileManifest {
	m := make(map[string]fileManifest, len(p.manifest.Files))
	for _, f := range p.manifest.Files {
		m[f.Path] = f
	}
	return m
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
	data, err := os.ReadFile(filepath.Join(dir, legacyChunksFile)) //nolint:gosec // dir derives from the repo-path sha256, not user input
	if err != nil {
		return chunkEnvelope{}, err
	}
	var envelope chunkEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return chunkEnvelope{}, fmt.Errorf("decode chunks: %w", err)
	}
	return envelope, nil
}
