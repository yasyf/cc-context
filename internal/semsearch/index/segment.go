package index

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/semsearch"
)

const (
	segmentsDir       = "segments"
	segmentFile       = "segment.json"
	segmentLock       = "segments"
	segmentTempPrefix = ".tmp-"
	segmentGrace      = time.Hour
	maxLayeredPercent = 10
)

type segmentEntry struct {
	Path   string `json:"path"`
	Hash   string `json:"sha256"`
	Offset int    `json:"offset"`
	Count  int    `json:"count"`
}

type segmentManifest struct {
	Files []segmentEntry `json:"files"`
}

type segmentFileID struct {
	path, hash string
	count      int
}

type openedSegment struct {
	chunks  chunkTable
	vectors vectorTable
	terms   *termStore
}

func segmentsFor(dir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(dir)), segmentsDir)
}

func segmentKey(variant string, entries []segmentEntry) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "segment %s\n", variant)
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\n", e.Path, e.Hash, e.Count)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mapSegment(dir, key string) (*openedSegment, []*mapping, error) {
	var maps []*mapping
	for _, name := range []string{chunksFile, vectorsFile, termsFile} {
		m, err := mapFile(filepath.Join(dir, name))
		if err != nil {
			unmapAll(maps)
			return nil, nil, err
		}
		maps = append(maps, m)
	}
	chunkGeneration, chunks, cerr := parseChunkTable(maps[0].data)
	vectorGeneration, vectors, verr := parseVectorTable(maps[1].data)
	termGeneration, terms, terr := readTermsMapped(maps[2].data)
	if err := errors.Join(cerr, verr, terr); err != nil {
		unmapAll(maps)
		return nil, nil, fmt.Errorf("segment %s: %w", key, err)
	}
	if chunkGeneration != key || vectorGeneration != key || termGeneration != key || chunks.n != vectors.rows || chunks.n != terms.nDocs {
		unmapAll(maps)
		return nil, nil, fmt.Errorf("segment %s: files disagree on generation or document count", key)
	}
	return &openedSegment{chunks: chunks, vectors: vectors, terms: terms}, maps, nil
}

func validSegment(dir, key string) bool {
	_, maps, err := mapSegment(dir, key)
	unmapAll(maps)
	return err == nil
}

func loadLayered(dir string, man manifest) *persisted {
	root := segmentsFor(dir)
	segments := make([]*openedSegment, len(man.Segments))
	var maps []*mapping
	for i, key := range man.Segments {
		s, m, err := mapSegment(filepath.Join(root, key), key)
		if err != nil {
			unmapAll(maps)
			return nil
		}
		segments[i] = s
		maps = append(maps, m...)
	}
	p := assembleLayered(man, segments)
	if p == nil {
		unmapAll(maps)
		return nil
	}
	p.files = &mappedFiles{maps: maps}
	return p
}

func assembleLayered(man manifest, segments []*openedSegment) *persisted {
	total := 0
	for _, f := range man.Files {
		if f.Count < 0 {
			return nil
		}
		total += f.Count
	}
	layers := make([]termLayer, len(segments))
	for i, s := range segments {
		toFlat := make([]int, s.chunks.n)
		for j := range toFlat {
			toFlat[j] = unusedDoc
		}
		layers[i] = termLayer{store: s.terms, toFlat: toFlat}
	}
	chunks := make([]semsearch.Chunk, 0, total)
	vectors := make([][]float32, 0, total)
	lengths := make([]int, 0, total)
	for _, f := range man.Files {
		if f.Segment < 0 || f.Segment >= len(segments) || f.Offset < 0 || f.Offset+f.Count > segments[f.Segment].chunks.n || f.Start != len(chunks) {
			return nil
		}
		s, layer := segments[f.Segment], layers[f.Segment]
		for j := f.Offset; j < f.Offset+f.Count; j++ {
			if layer.toFlat[j] != unusedDoc {
				return nil
			}
			layer.toFlat[j] = len(chunks)
			chunks = append(chunks, s.chunks.at(j, f.Path))
			vectors = append(vectors, s.vectors.row(j))
			lengths = append(lengths, s.terms.length(j))
		}
	}
	if !layersAscend(layers) {
		return nil
	}
	return &persisted{manifest: man, chunks: chunks, vectors: vectors, terms: &corpusTerms{lengths: lengths, layers: layers}}
}

func layersAscend(layers []termLayer) bool {
	for _, l := range layers {
		prev := unusedDoc
		for _, flat := range l.toFlat {
			if flat == unusedDoc {
				continue
			}
			if flat <= prev {
				return false
			}
			prev = flat
		}
	}
	return true
}

type stagedSegment struct {
	key, tmp string
}

func placeFiles(root, variant string, files []fileManifest, chunks []semsearch.Chunk, vectors [][]float32, candidates []string) ([]fileManifest, []string, []stagedSegment, error) {
	base, offsets := chooseBase(root, files, len(chunks), candidates)
	if base == "" {
		return stageWhole(root, variant, files, chunks, vectors)
	}
	placed := append([]fileManifest(nil), files...)
	var overlay []segmentEntry
	var overlayChunks []semsearch.Chunk
	var overlayVectors [][]float32
	for i, f := range placed {
		if off, ok := offsets[segmentFileID{f.Path, f.Hash, f.Count}]; ok {
			placed[i].Segment, placed[i].Offset = 0, off
			continue
		}
		placed[i].Segment, placed[i].Offset = 1, len(overlayChunks)
		overlay = append(overlay, segmentEntry{Path: f.Path, Hash: f.Hash, Offset: len(overlayChunks), Count: f.Count})
		overlayChunks = append(overlayChunks, chunks[f.Start:f.Start+f.Count]...)
		overlayVectors = append(overlayVectors, vectors[f.Start:f.Start+f.Count]...)
	}
	if len(overlay) == 0 {
		return placed, []string{base}, nil, nil
	}
	key := segmentKey(variant, overlay)
	staged, err := stageSegment(root, key, overlay, overlayChunks, overlayVectors)
	if err != nil {
		return nil, nil, nil, err
	}
	return placed, []string{base, key}, staged, nil
}

func chooseBase(root string, files []fileManifest, live int, candidates []string) (string, map[segmentFileID]int) {
	best, bestCovered := "", 0
	var bestOffsets map[segmentFileID]int
	seen := map[string]bool{}
	for _, key := range candidates {
		if seen[key] {
			continue
		}
		seen[key] = true
		seg, err := readSegmentManifest(filepath.Join(root, key))
		if err != nil {
			continue
		}
		offsets := make(map[segmentFileID]int, len(seg.Files))
		total := 0
		for _, e := range seg.Files {
			offsets[segmentFileID{e.Path, e.Hash, e.Count}] = e.Offset
			total += e.Count
		}
		covered := 0
		for _, f := range files {
			if _, ok := offsets[segmentFileID{f.Path, f.Hash, f.Count}]; ok {
				covered += f.Count
			}
		}
		overlay, tombstoned := live-covered, total-covered
		if overlay*100 > maxLayeredPercent*live || tombstoned*100 > maxLayeredPercent*total {
			continue
		}
		if covered > bestCovered {
			best, bestCovered, bestOffsets = key, covered, offsets
		}
	}
	return best, bestOffsets
}

func stageWhole(root, variant string, files []fileManifest, chunks []semsearch.Chunk, vectors [][]float32) ([]fileManifest, []string, []stagedSegment, error) {
	placed := append([]fileManifest(nil), files...)
	entries := make([]segmentEntry, len(placed))
	for i, f := range placed {
		placed[i].Segment, placed[i].Offset = 0, f.Start
		entries[i] = segmentEntry{Path: f.Path, Hash: f.Hash, Offset: f.Start, Count: f.Count}
	}
	key := segmentKey(variant, entries)
	staged, err := stageSegment(root, key, entries, chunks, vectors)
	if err != nil {
		return nil, nil, nil, err
	}
	return placed, []string{key}, staged, nil
}

func stageSegment(root, key string, entries []segmentEntry, chunks []semsearch.Chunk, vectors [][]float32) ([]stagedSegment, error) {
	if validSegment(filepath.Join(root, key), key) {
		return nil, nil
	}
	tmp, err := os.MkdirTemp(root, segmentTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("create segment dir: %w", err)
	}
	if err := writeSegment(tmp, key, entries, chunks, vectors); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	return []stagedSegment{{key: key, tmp: tmp}}, nil
}

func installSegments(root string, staged []stagedSegment) error {
	for _, s := range staged {
		final := filepath.Join(root, s.key)
		if validSegment(final, s.key) {
			continue
		}
		if err := os.RemoveAll(final); err != nil {
			return fmt.Errorf("remove invalid segment %s: %w", s.key, err)
		}
		if err := os.Rename(s.tmp, final); err != nil {
			return fmt.Errorf("install segment %s: %w", s.key, err)
		}
	}
	return nil
}

func writeSegment(dir, key string, entries []segmentEntry, chunks []semsearch.Chunk, vectors [][]float32) error {
	segData, err := json.Marshal(segmentManifest{Files: entries})
	if err != nil {
		return fmt.Errorf("marshal segment manifest: %w", err)
	}
	writers := []struct {
		name  string
		write func(io.Writer) error
	}{
		{chunksFile, func(w io.Writer) error { return writeChunks(w, key, chunks) }},
		{vectorsFile, func(w io.Writer) error { return writeVectors(w, key, vectors) }},
		{termsFile, func(w io.Writer) error { return writeTerms(w, key, chunks) }},
	}
	for _, f := range writers {
		if err := cache.StoreFrom(filepath.Join(dir, f.name), 0o640, f.write); err != nil {
			return err
		}
	}
	return cache.Store(filepath.Join(dir, segmentFile), segData, 0o640)
}

func readSegmentManifest(dir string) (segmentManifest, error) {
	var seg segmentManifest
	data, err := os.ReadFile(filepath.Join(dir, segmentFile)) //nolint:gosec // dir is a segment key under the trusted cache dir
	if err != nil {
		return seg, err
	}
	if err := json.Unmarshal(data, &seg); err != nil {
		return seg, fmt.Errorf("decode segment manifest: %w", err)
	}
	return seg, nil
}

func segmentsValid(root string, keys []string) bool {
	for _, key := range keys {
		if !validSegment(filepath.Join(root, key), key) {
			return false
		}
	}
	return true
}

func store(ctx context.Context, dir string, man manifest, chunks []semsearch.Chunk, vectors [][]float32, candidates []string) error {
	root := segmentsFor(dir)
	for _, d := range []string{dir, root} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create cache dir %q: %w", d, err)
		}
	}
	variant := filepath.Base(dir)
	files, segments, staged, err := placeFiles(root, variant, man.Files, chunks, vectors, candidates)
	if err != nil {
		return err
	}
	defer func() {
		for _, s := range staged {
			_ = os.RemoveAll(s.tmp)
		}
	}()
	return cache.WithLock(ctx, root, segmentLock, func() error {
		if err := installSegments(root, staged); err != nil {
			return err
		}
		if !segmentsValid(root, segments) {
			var whole []stagedSegment
			if files, segments, whole, err = stageWhole(root, variant, man.Files, chunks, vectors); err != nil {
				return err
			}
			staged = append(staged, whole...)
			if err := installSegments(root, whole); err != nil {
				return err
			}
		}
		man.Files, man.Segments = files, segments
		return writeManifest(dir, man)
	})
}

func writeManifest(dir string, man manifest) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate manifest generation: %w", err)
	}
	man.Generation = hex.EncodeToString(nonce[:])
	data, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return cache.Store(filepath.Join(dir, manifestFile), data, 0o640)
}

func manifestSegments(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is a manifest under the trusted cache dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("decode manifest %s: not an object", path)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode manifest %s: %w", path, err)
		}
		switch tok {
		case "segments":
			var segments []string
			if err := dec.Decode(&segments); err != nil {
				return nil, fmt.Errorf("decode manifest %s: %w", path, err)
			}
			return segments, nil
		case "files":
			return nil, nil
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, fmt.Errorf("decode manifest %s: %w", path, err)
		}
	}
	return nil, nil
}

func familySegments(dirs []string) []string {
	var keys []string
	for _, dir := range dirs {
		segments, err := manifestSegments(filepath.Join(dir, manifestFile))
		if err == nil {
			keys = append(keys, segments...)
		}
	}
	return keys
}

func sweepSegments(ctx context.Context, base string) {
	root := filepath.Join(base, segmentsDir)
	if _, err := os.Stat(root); err != nil {
		return
	}
	lockCtx, cancel := context.WithTimeout(ctx, pruneLockWait)
	defer cancel()
	_ = cache.WithLock(lockCtx, root, segmentLock, func() error {
		referenced, err := referencedSegments(base)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		cutoff := time.Now().Add(-segmentGrace)
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || referenced[name] || (!isKey(name) && !strings.HasPrefix(name, segmentTempPrefix)) {
				continue
			}
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				_ = os.RemoveAll(filepath.Join(root, name))
			}
		}
		return nil
	})
}

func referencedSegments(base string) (map[string]bool, error) {
	referenced := map[string]bool{}
	repos, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if !repo.IsDir() || !isKey(repo.Name()) {
			continue
		}
		variants, err := os.ReadDir(filepath.Join(base, repo.Name()))
		if err != nil {
			return nil, err
		}
		for _, v := range variants {
			if !v.IsDir() {
				continue
			}
			segments, err := manifestSegments(filepath.Join(base, repo.Name(), v.Name(), manifestFile))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				return nil, err
			}
			for _, key := range segments {
				referenced[key] = true
			}
		}
	}
	return referenced, nil
}
