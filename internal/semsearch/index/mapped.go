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
	"syscall"
	"unsafe"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

type mapping struct {
	data []byte
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
	data, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED) //nolint:gosec // an fd always fits an int
	if err != nil {
		return nil, fmt.Errorf("map %s: %w", path, err)
	}
	return &mapping{data: data}, nil
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
		_ = syscall.Munmap(m.data)
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
	if r.err == nil && n > uint64(len(r.data)-r.off)/uint64(width) {
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

func readChunksMapped(data []byte, files []fileManifest) (string, []semsearch.Chunk, error) {
	r := &byteReader{data: data}
	generation := r.generation()
	n := r.count(16)
	starts, ends, offsets := r.take(4*n), r.take(4*n), r.take(8*(n+1))
	blob := r.data[min(r.off, len(r.data)):]
	if r.err != nil {
		return "", nil, fmt.Errorf("chunks.bin: %w", r.err)
	}
	if !ascendingTo(offsets, uint64(len(blob))) {
		return "", nil, errors.New("chunks.bin: content offsets are not ascending to the end of the file")
	}
	chunks := make([]semsearch.Chunk, n)
	for i := range chunks {
		lo := binary.LittleEndian.Uint64(offsets[8*i:])
		hi := binary.LittleEndian.Uint64(offsets[8*i+8:])
		chunks[i] = semsearch.Chunk{
			StartLine: int(binary.LittleEndian.Uint32(starts[4*i:])),
			EndLine:   int(binary.LittleEndian.Uint32(ends[4*i:])),
			Content:   aliasString(blob[lo:hi]),
		}
	}
	for _, f := range files {
		if f.Start < 0 || f.Count < 0 || f.Start+f.Count > n {
			return "", nil, fmt.Errorf("chunks.bin: %s spans [%d, %d) of %d chunks", f.Path, f.Start, f.Start+f.Count, n)
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
	return unsafe.String(&b[0], len(b))
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
		if _, err := w.Write(unsafe.Slice((*byte)(unsafe.Pointer(&row[0])), 4*len(row))); err != nil {
			return err
		}
	}
	return nil
}

func readVectorsMapped(data []byte) (string, [][]float32, error) {
	r := &byteReader{data: data}
	generation := r.generation()
	rows, dims := int(r.u32()), int(r.u32())
	if r.err != nil {
		return "", nil, fmt.Errorf("vectors.bin: %w", r.err)
	}
	if want := r.off + rows*dims*4; len(data) != want {
		return "", nil, fmt.Errorf("vectors.bin is %d bytes, want %d (rows=%d dims=%d)", len(data), want, rows, dims)
	}
	if r.off%4 != 0 {
		return "", nil, fmt.Errorf("vectors.bin matrix starts at unaligned offset %d", r.off)
	}
	out := make([][]float32, rows)
	if rows == 0 || dims == 0 {
		return generation, out, nil
	}
	flat := unsafe.Slice((*float32)(unsafe.Pointer(&data[r.off])), rows*dims)
	for i := range out {
		out[i] = flat[i*dims : (i+1)*dims : (i+1)*dims]
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
	lengths        []int
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
	lengthBytes := r.take(4 * nDocs)
	s := &termStore{nTerms: nTerms}
	s.termOffsets = r.take(8 * (nTerms + 1))
	s.postingOffsets = r.take(8 * (nTerms + 1))
	if r.err != nil {
		return "", nil, fmt.Errorf("terms.bin: %w", r.err)
	}
	termBytes := binary.LittleEndian.Uint64(s.termOffsets[8*nTerms:])
	if termBytes > uint64(len(data)-r.off) {
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
	if !ascendingTo(s.termOffsets, termBytes) || !ascendingTo(s.postingOffsets, uint64(nPostings)) {
		return "", nil, errors.New("terms.bin: offset tables are not ascending to their section ends")
	}
	for i := 1; i < nTerms; i++ {
		if s.term(i-1) >= s.term(i) {
			return "", nil, errors.New("terms.bin: term table is not strictly ascending")
		}
	}
	s.lengths = make([]int, nDocs)
	for i := range s.lengths {
		s.lengths[i] = int(binary.LittleEndian.Uint32(lengthBytes[4*i:]))
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

func (s *termStore) bm25(query string) (*rank.BM25, error) {
	terms := map[string][]rank.BM25Posting{}
	for _, t := range rank.Tokenize(query) {
		if _, seen := terms[t]; seen {
			continue
		}
		terms[t] = s.lookup(t)
	}
	for t, p := range terms {
		if p == nil {
			delete(terms, t)
		}
	}
	return rank.BM25FromPostings(s.lengths, terms)
}
