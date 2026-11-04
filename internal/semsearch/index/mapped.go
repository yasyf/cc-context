package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"unsafe"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

type mapping struct {
	data  []byte
	unmap func() error
}

func mapFile(path string) (*mapping, error) {
	f, err := os.Open(path) //nolint:gosec // path is under the trusted cache dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	data, unmap, err := mapOwnedFile(f, int(fi.Size()))
	if err != nil {
		return nil, fmt.Errorf("map %s: %w", path, err)
	}
	return &mapping{data: data, unmap: unmap}, nil
}

// mappedFiles owns the mapped cache files an index reads in place. Chunk
// contents and vector rows alias these mappings rather than the Go heap, so
// nothing may read them once unmap runs.
type mappedFiles struct {
	maps []*mapping
}

func (m *mappedFiles) unmap() {
	if m == nil {
		return
	}
	unmapAll(m.maps)
	m.maps = nil
}

func unmapAll(maps []*mapping) {
	for _, m := range maps {
		_ = m.unmap()
	}
}

type byteReader struct {
	data []byte
	off  int
	err  error
}

func (r *byteReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.data)-r.off {
		r.err = errors.New("truncated")
		return nil
	}
	b := r.data[r.off : r.off+n : r.off+n]
	r.off += n
	return b
}

func (r *byteReader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *byteReader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *byteReader) count(width int) int {
	n := r.u64()
	if r.err == nil && n > uint64(len(r.data)-r.off)/uint64(width) { //nolint:gosec // len minus a read offset that never passes it is non-negative
		r.err = fmt.Errorf("count %d exceeds the remaining %d bytes", n, len(r.data)-r.off)
	}
	return int(n) //nolint:gosec // bounded by the mapping length above
}

func (r *byteReader) generation() string {
	return string(r.take(int(r.u32())))
}

func writeGeneration(w io.Writer, generation string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(generation))); err != nil { //nolint:gosec // generated nonce length fits the u32 framing
		return err
	}
	_, err := io.WriteString(w, generation)
	return err
}

func writeU64(w io.Writer, v uint64) error { return binary.Write(w, binary.LittleEndian, v) }

func writeU32s(w io.Writer, vs []uint32) error { return binary.Write(w, binary.LittleEndian, vs) }

func writeU64s(w io.Writer, vs []uint64) error { return binary.Write(w, binary.LittleEndian, vs) }

func writeChunks(w io.Writer, generation string, chunks []semsearch.Chunk) error {
	if err := writeGeneration(w, generation); err != nil {
		return err
	}
	n := len(chunks)
	if err := writeU64(w, uint64(n)); err != nil {
		return err
	}
	starts := make([]uint32, n)
	ends := make([]uint32, n)
	offsets := make([]uint64, n+1)
	for i, c := range chunks {
		starts[i], ends[i] = uint32(c.StartLine), uint32(c.EndLine) //nolint:gosec // line numbers fit u32
		offsets[i+1] = offsets[i] + uint64(len(c.Content))
	}
	if err := writeU32s(w, starts); err != nil {
		return err
	}
	if err := writeU32s(w, ends); err != nil {
		return err
	}
	if err := writeU64s(w, offsets); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := io.WriteString(w, c.Content); err != nil {
			return err
		}
	}
	return nil
}

type chunkTable struct {
	n                     int
	starts, ends, offsets []byte
	blob                  []byte
}

func parseChunkTable(data []byte) (string, chunkTable, error) {
	r := &byteReader{data: data}
	generation := r.generation()
	n := r.count(16)
	t := chunkTable{n: n, starts: r.take(4 * n), ends: r.take(4 * n), offsets: r.take(8 * (n + 1))}
	t.blob = r.data[min(r.off, len(r.data)):]
	if r.err != nil {
		return "", chunkTable{}, fmt.Errorf("chunks.bin: %w", r.err)
	}
	if !ascendingTo(t.offsets, uint64(len(t.blob))) {
		return "", chunkTable{}, errors.New("chunks.bin: content offsets are not ascending to the end of the file")
	}
	return generation, t, nil
}

func (t chunkTable) at(i int, path string) semsearch.Chunk {
	lo := binary.LittleEndian.Uint64(t.offsets[8*i:])
	hi := binary.LittleEndian.Uint64(t.offsets[8*i+8:])
	return semsearch.Chunk{
		Path:      path,
		StartLine: int(binary.LittleEndian.Uint32(t.starts[4*i:])),
		EndLine:   int(binary.LittleEndian.Uint32(t.ends[4*i:])),
		Content:   aliasString(t.blob[lo:hi]),
	}
}

func readChunksMapped(data []byte, files []fileManifest) (string, []semsearch.Chunk, error) {
	generation, t, err := parseChunkTable(data)
	if err != nil {
		return "", nil, err
	}
	chunks := make([]semsearch.Chunk, t.n)
	for i := range chunks {
		chunks[i] = t.at(i, "")
	}
	for _, f := range files {
		if f.Start < 0 || f.Count < 0 || f.Start+f.Count > t.n {
			return "", nil, fmt.Errorf("chunks.bin: %s spans [%d, %d) of %d chunks", f.Path, f.Start, f.Start+f.Count, t.n)
		}
		for i := f.Start; i < f.Start+f.Count; i++ {
			chunks[i].Path = f.Path
		}
	}
	return generation, chunks, nil
}

func aliasString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b)) //nolint:gosec // aliases the read-only mapping the index owns until Close
}

func writeVectors(w io.Writer, generation string, vectors [][]float32) error {
	dims := 0
	if len(vectors) > 0 {
		dims = len(vectors[0])
	}
	if err := writeGeneration(w, generation); err != nil {
		return err
	}
	if err := writeU32s(w, []uint32{uint32(len(vectors)), uint32(dims)}); err != nil { //nolint:gosec // matrix dims fit the u32 framing
		return err
	}
	for _, row := range vectors {
		if len(row) == 0 {
			continue
		}
		if _, err := w.Write(unsafe.Slice((*byte)(unsafe.Pointer(&row[0])), 4*len(row))); err != nil { //nolint:gosec // a row is a contiguous float32 slice written in host little-endian order
			return err
		}
	}
	return nil
}

type vectorTable struct {
	rows, dims int
	flat       []float32
}

func parseVectorTable(data []byte) (string, vectorTable, error) {
	r := &byteReader{data: data}
	generation := r.generation()
	rows, dims := int(r.u32()), int(r.u32())
	if r.err != nil {
		return "", vectorTable{}, fmt.Errorf("vectors.bin: %w", r.err)
	}
	if want := r.off + rows*dims*4; len(data) != want {
		return "", vectorTable{}, fmt.Errorf("vectors.bin is %d bytes, want %d (rows=%d dims=%d)", len(data), want, rows, dims)
	}
	if r.off%4 != 0 {
		return "", vectorTable{}, fmt.Errorf("vectors.bin matrix starts at unaligned offset %d", r.off)
	}
	t := vectorTable{rows: rows, dims: dims}
	if rows > 0 && dims > 0 {
		t.flat = unsafe.Slice((*float32)(unsafe.Pointer(&data[r.off])), rows*dims) //nolint:gosec // vectors.bin rows alias the read-only mapping the index owns until Close
	}
	return generation, t, nil
}

func (t vectorTable) row(i int) []float32 {
	if t.dims == 0 {
		return nil
	}
	return t.flat[i*t.dims : (i+1)*t.dims : (i+1)*t.dims]
}

func readVectorsMapped(data []byte) (string, [][]float32, error) {
	generation, t, err := parseVectorTable(data)
	if err != nil {
		return "", nil, err
	}
	out := make([][]float32, t.rows)
	for i := range out {
		out[i] = t.row(i)
	}
	return generation, out, nil
}

func writeTerms(w io.Writer, generation string, chunks []semsearch.Chunk) error {
	lengths := make([]uint32, len(chunks))
	postings := map[string][]uint64{}
	total := 0
	for i, c := range chunks {
		tokens := rank.Tokenize(rank.EnrichForBM25(c))
		lengths[i] = uint32(len(tokens)) //nolint:gosec // a chunk's token count fits u32
		counts := map[string]uint32{}
		for _, t := range tokens {
			counts[t]++
		}
		for t, tf := range counts {
			list, ok := postings[t]
			if !ok {
				t = strings.Clone(t)
			}
			postings[t] = append(list, uint64(i)<<32|uint64(tf)) //nolint:gosec // chunk index fits u32
			total++
		}
	}
	terms := slices.Sorted(maps.Keys(postings))

	if err := writeGeneration(w, generation); err != nil {
		return err
	}
	if err := writeU64s(w, []uint64{uint64(len(chunks)), uint64(len(terms)), uint64(total)}); err != nil {
		return err
	}
	if err := writeU32s(w, lengths); err != nil {
		return err
	}
	termOffsets := make([]uint64, len(terms)+1)
	postingOffsets := make([]uint64, len(terms)+1)
	for i, t := range terms {
		termOffsets[i+1] = termOffsets[i] + uint64(len(t))
		postingOffsets[i+1] = postingOffsets[i] + uint64(len(postings[t]))
	}
	if err := writeU64s(w, termOffsets); err != nil {
		return err
	}
	if err := writeU64s(w, postingOffsets); err != nil {
		return err
	}
	for _, t := range terms {
		if _, err := io.WriteString(w, t); err != nil {
			return err
		}
	}
	var buf []byte
	for _, t := range terms {
		buf = buf[:0]
		for _, p := range postings[t] {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(p>>32)) //nolint:gosec // the doc half packed above
			buf = binary.LittleEndian.AppendUint32(buf, uint32(p))     //nolint:gosec // the tf half packed above
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

type termStore struct {
	nDocs          int
	lengths        []byte
	termOffsets    []byte
	postingOffsets []byte
	terms          []byte
	postings       []byte
	nTerms         int
}

func readTermsMapped(data []byte) (string, *termStore, error) {
	r := &byteReader{data: data}
	generation := r.generation()
	nDocs := r.count(4)
	nTerms := r.count(16)
	nPostings := r.count(8)
	s := &termStore{nDocs: nDocs, nTerms: nTerms, lengths: r.take(4 * nDocs)}
	s.termOffsets = r.take(8 * (nTerms + 1))
	s.postingOffsets = r.take(8 * (nTerms + 1))
	if r.err != nil {
		return "", nil, fmt.Errorf("terms.bin: %w", r.err)
	}
	termBytes := binary.LittleEndian.Uint64(s.termOffsets[8*nTerms:])
	if termBytes > uint64(len(data)-r.off) { //nolint:gosec // len minus a read offset that never passes it is non-negative
		return "", nil, fmt.Errorf("terms.bin: %d term bytes exceed the file", termBytes)
	}
	s.terms = r.take(int(termBytes)) //nolint:gosec // bounded by the file length above
	s.postings = r.take(8 * nPostings)
	if r.err != nil {
		return "", nil, fmt.Errorf("terms.bin: %w", r.err)
	}
	if r.off != len(data) {
		return "", nil, errors.New("terms.bin: trailing bytes after the postings")
	}
	if !ascendingTo(s.termOffsets, termBytes) || !ascendingTo(s.postingOffsets, uint64(nPostings)) { //nolint:gosec // a count bounded by the file length is non-negative
		return "", nil, errors.New("terms.bin: offset tables are not ascending to their section ends")
	}
	for i := 1; i < nTerms; i++ {
		if s.term(i-1) >= s.term(i) {
			return "", nil, errors.New("terms.bin: term table is not strictly ascending")
		}
	}
	return generation, s, nil
}

func ascendingTo(offsets []byte, end uint64) bool {
	prev := uint64(0)
	for i := 0; i < len(offsets); i += 8 {
		v := binary.LittleEndian.Uint64(offsets[i:])
		if v < prev || v > end {
			return false
		}
		prev = v
	}
	return binary.LittleEndian.Uint64(offsets[0:]) == 0 && prev == end
}

func (s *termStore) length(i int) int {
	return int(binary.LittleEndian.Uint32(s.lengths[4*i:]))
}

func (s *termStore) term(i int) string {
	lo := binary.LittleEndian.Uint64(s.termOffsets[8*i:])
	hi := binary.LittleEndian.Uint64(s.termOffsets[8*i+8:])
	return aliasString(s.terms[lo:hi])
}

func (s *termStore) lookup(term string) []rank.BM25Posting {
	i := sort.Search(s.nTerms, func(j int) bool { return s.term(j) >= term })
	if i == s.nTerms || s.term(i) != term {
		return nil
	}
	from := binary.LittleEndian.Uint64(s.postingOffsets[8*i:])
	to := binary.LittleEndian.Uint64(s.postingOffsets[8*i+8:])
	out := make([]rank.BM25Posting, to-from)
	for j := range out {
		p := s.postings[8*(from+uint64(j)):] //nolint:gosec // j indexes a slice sized from the same range
		out[j] = rank.BM25Posting{
			Document:  int(binary.LittleEndian.Uint32(p)),
			Frequency: int(binary.LittleEndian.Uint32(p[4:])),
		}
	}
	return out
}

type corpusTerms struct {
	lengths []int
	layers  []termLayer
}

type termLayer struct {
	store  *termStore
	toFlat []int
}

const unusedDoc = -1

func (c *corpusTerms) bm25(query string) (*rank.BM25, error) {
	terms := map[string][]rank.BM25Posting{}
	for _, t := range rank.Tokenize(query) {
		if _, seen := terms[t]; seen {
			continue
		}
		var merged []rank.BM25Posting
		for _, l := range c.layers {
			postings, err := l.postings(t)
			if err != nil {
				return nil, err
			}
			merged = mergePostings(merged, postings)
		}
		terms[t] = merged
	}
	for t, p := range terms {
		if p == nil {
			delete(terms, t)
		}
	}
	return rank.BM25FromPostings(c.lengths, terms)
}

func (l termLayer) postings(term string) ([]rank.BM25Posting, error) {
	raw := l.store.lookup(term)
	out := raw[:0]
	for _, p := range raw {
		if p.Document < 0 || p.Document >= len(l.toFlat) {
			return nil, fmt.Errorf("terms.bin: posting for %q names document %d of %d", term, p.Document, len(l.toFlat))
		}
		if flat := l.toFlat[p.Document]; flat != unusedDoc {
			p.Document = flat
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func mergePostings(a, b []rank.BM25Posting) []rank.BM25Posting {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]rank.BM25Posting, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		if a[0].Document <= b[0].Document {
			out, a = append(out, a[0]), a[1:]
		} else {
			out, b = append(out, b[0]), b[1:]
		}
	}
	return append(append(out, a...), b...)
}
