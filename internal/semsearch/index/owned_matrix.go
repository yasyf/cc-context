package index

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"unsafe"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

const (
	ownedMatrixMagic  = "CCXOWN03"
	ownedMatrixHeader = 128
	ownedMatrixRow    = 40
	ownedMatrixTerm   = 32
	ownedMatrixPost   = 16
)

// OwnedAnswerWork separates publication materialization from mapped bytes and query arithmetic.
type OwnedAnswerWork struct {
	MaterializationSQLRows            int
	MaterializedChunks int
	DecodedValues      int
	PublishedBytes     int
	MappedBytes        int
}

// OwnedAnswerIndex owns a read-only generation mapping; Close ends every borrowed view.
type OwnedAnswerIndex struct {
	Reindexed  int
	TotalFiles int
	Work       OwnedAnswerWork
	data       []byte
	vectors    []float32
	rows       int
	dims       int
	rowOffset  int
	termOffset int
	terms      int
	postOffset int
	textOffset int
	totalLen   int
	unmap      func() error
}

type ownedAnswerMaterialization struct {
	index   *Index
	lengths []int
	terms   map[string][]rank.BM25Posting
	work    OwnedAnswerWork
}

type mappedPostings struct{ data []byte }

func (p mappedPostings) Len() int { return len(p.data) / ownedMatrixPost }

func (p mappedPostings) At(i int) rank.BM25Posting {
	row := p.data[i*ownedMatrixPost:]
	return rank.BM25Posting{
		Document:  ownedMatrixInt(binary.NativeEndian.Uint64(row)),
		Frequency: ownedMatrixInt(binary.NativeEndian.Uint64(row[8:])),
	}
}

func (m *OwnedAnswerIndex) Len() int { return m.rows }

func (m *OwnedAnswerIndex) OrderSize() int { return m.rows }

func (m *OwnedAnswerIndex) TotalLength() int { return m.totalLen }

func (m *OwnedAnswerIndex) Position(i int) int { return i }

func (m *OwnedAnswerIndex) DocumentLength(i int) int {
	return ownedMatrixInt(binary.NativeEndian.Uint64(m.data[m.rowOffset+i*ownedMatrixRow+32:]))
}

// Chunk borrows its path from the mapping and carries no canonical body text.
func (m *OwnedAnswerIndex) Chunk(i int) semsearch.Chunk {
	row := m.data[m.rowOffset+i*ownedMatrixRow:]
	return semsearch.Chunk{
		Path:      m.text(binary.NativeEndian.Uint64(row), binary.NativeEndian.Uint64(row[8:])),
		StartLine: ownedMatrixInt(binary.NativeEndian.Uint64(row[16:])),
		EndLine:   ownedMatrixInt(binary.NativeEndian.Uint64(row[24:])),
	}
}

// Vector borrows an immutable row that remains valid until Close.
func (m *OwnedAnswerIndex) Vector(i int) []float32 {
	return m.vectors[i*m.dims : (i+1)*m.dims : (i+1)*m.dims]
}

func (m *OwnedAnswerIndex) text(offset, length uint64) string {
	start := m.textOffset + ownedMatrixInt(offset)
	data := m.data[start : start+ownedMatrixInt(length)]
	return unsafe.String(unsafe.SliceData(data), len(data))
}

func (m *OwnedAnswerIndex) Postings(term string) rank.BM25Postings {
	i := sort.Search(m.terms, func(i int) bool {
		row := m.data[m.termOffset+i*ownedMatrixTerm:]
		return m.text(binary.NativeEndian.Uint64(row), binary.NativeEndian.Uint64(row[8:])) >= term
	})
	if i == m.terms {
		return mappedPostings{}
	}
	row := m.data[m.termOffset+i*ownedMatrixTerm:]
	if m.text(binary.NativeEndian.Uint64(row), binary.NativeEndian.Uint64(row[8:])) != term {
		return mappedPostings{}
	}
	start := ownedMatrixInt(binary.NativeEndian.Uint64(row[16:]))
	count := ownedMatrixInt(binary.NativeEndian.Uint64(row[24:]))
	return mappedPostings{data: m.data[m.postOffset+start*ownedMatrixPost : m.postOffset+(start+count)*ownedMatrixPost]}
}

func (m *OwnedAnswerIndex) GetScores(tokens []string) []float64 {
	return rank.ScoreBM25(tokens, m)
}

// Close releases the mapping after callers have copied any selected paths they retain.
func (m *OwnedAnswerIndex) Close() error {
	err := m.unmap()
	m.data = nil
	m.vectors = nil
	m.unmap = nil
	return err
}

func mapOwnedAnswers(ctx context.Context, path, identity, generation string, documents, dims int) (*OwnedAnswerIndex, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	if info.Size() < ownedMatrixHeader || info.Size() > math.MaxInt {
		return nil, false, errors.Join(fmt.Errorf("owned answer matrix has invalid size %d", info.Size()), file.Close())
	}
	data, unmap, err := mapOwnedFile(file, int(info.Size()))
	closeErr := file.Close()
	if err != nil {
		return nil, false, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, false, errors.Join(closeErr, unmap())
	}
	index, err := decodeOwnedAnswers(data, identity, generation, documents, dims)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, false, errors.Join(err, unmap())
	}
	index.unmap = unmap
	return index, true, nil
}

func decodeOwnedAnswers(data []byte, identity, generation string, documents, dims int) (*OwnedAnswerIndex, error) {
	if string(data[:8]) != ownedMatrixMagic {
		return nil, fmt.Errorf("owned answer matrix header mismatch")
	}
	var header [16]int
	for i := 1; i < len(header); i++ {
		value := binary.NativeEndian.Uint64(data[i*8:])
		if value > math.MaxInt {
			return nil, fmt.Errorf("owned answer matrix integer exceeds native range")
		}
		header[i] = int(value)
	}
	if header[1] != len(data) {
		return nil, fmt.Errorf("owned answer matrix size mismatch")
	}
	identityEnd, err := ownedMatrixExtent(ownedMatrixHeader, header[2], 1, len(data))
	if err != nil {
		return nil, err
	}
	generationEnd, err := ownedMatrixExtent(identityEnd, header[3], 1, len(data))
	if err != nil {
		return nil, err
	}
	if string(data[ownedMatrixHeader:identityEnd]) != identity || string(data[identityEnd:generationEnd]) != generation || header[4] != documents || header[6] != dims {
		return nil, fmt.Errorf("owned answer matrix identity mismatch")
	}
	rows := header[5]
	if rows == 0 || rows > math.MaxInt32 || header[8] != alignOwnedMatrix(generationEnd) {
		return nil, fmt.Errorf("owned answer matrix rows invalid")
	}
	rowEnd, err := ownedMatrixExtent(header[8], rows, ownedMatrixRow, len(data))
	if err != nil {
		return nil, err
	}
	if header[9] != rowEnd {
		return nil, fmt.Errorf("owned answer matrix row extent invalid")
	}
	if dims < 0 || (dims != 0 && rows > (len(data)-rowEnd)/4/dims) {
		return nil, fmt.Errorf("owned answer matrix dimensions invalid")
	}
	values := rows * dims
	vectorEnd := rowEnd + values*4
	if header[10] != alignOwnedMatrix(vectorEnd) {
		return nil, fmt.Errorf("owned answer matrix vector extent invalid")
	}
	termEnd, err := ownedMatrixExtent(header[10], header[11], ownedMatrixTerm, len(data))
	if err != nil {
		return nil, err
	}
	if header[12] != termEnd {
		return nil, fmt.Errorf("owned answer matrix term extent invalid")
	}
	postEnd, err := ownedMatrixExtent(header[12], header[13], ownedMatrixPost, len(data))
	if err != nil {
		return nil, err
	}
	if header[14] != postEnd {
		return nil, fmt.Errorf("owned answer matrix posting extent invalid")
	}
	m := &OwnedAnswerIndex{
		TotalFiles: documents,
		Work: OwnedAnswerWork{MappedBytes: len(data)},
		data: data, rows: rows, dims: dims,
		rowOffset: header[8], termOffset: header[10], terms: header[11],
		postOffset: header[12], textOffset: header[14], totalLen: header[7],
	}
	m.vectors = unsafe.Slice((*float32)(unsafe.Pointer(unsafe.SliceData(data[rowEnd:vectorEnd]))), values)
	return m, nil
}

func ownedMatrixExtent(offset, count, width, size int) (int, error) {
	if offset < 0 || offset > size || count < 0 || count > (size-offset)/width {
		return 0, fmt.Errorf("owned answer matrix section exceeds its file")
	}
	return offset + count*width, nil
}

func ownedMatrixInt(value uint64) int {
	if value > math.MaxInt {
		panic("owned answer matrix integer exceeds native range")
	}
	return int(value)
}

func alignOwnedMatrix(offset int) int { return (offset + 7) &^ 7 }

func encodeOwnedAnswers(ctx context.Context, identity, generation string, source *ownedAnswerMaterialization, dims int) ([]byte, error) {
	keys := make([]string, 0, len(source.terms))
	posts := 0
	for term, values := range source.terms {
		keys = append(keys, term)
		posts += len(values)
	}
	slices.Sort(keys)
	rows := len(source.index.Chunks)
	rowOffset := alignOwnedMatrix(ownedMatrixHeader + len(identity) + len(generation))
	vectorOffset := rowOffset + rows*ownedMatrixRow
	termOffset := alignOwnedMatrix(vectorOffset + rows*dims*4)
	postOffset := termOffset + len(keys)*ownedMatrixTerm
	textOffset := postOffset + posts*ownedMatrixPost
	data := make([]byte, textOffset)
	copy(data, ownedMatrixMagic)
	put := func(offset int, value int) {
		if value < 0 {
			panic("negative owned answer matrix value")
		}
		binary.NativeEndian.PutUint64(data[offset:], uint64(value))
	}
	header := func(field, value int) { put(field*8, value) }
	header(2, len(identity))
	header(3, len(generation))
	header(4, source.index.TotalFiles)
	header(5, rows)
	header(6, dims)
	total := 0
	for _, length := range source.lengths {
		total += length
	}
	header(7, total)
	header(8, rowOffset)
	header(9, vectorOffset)
	header(10, termOffset)
	header(11, len(keys))
	header(12, postOffset)
	header(13, posts)
	header(14, textOffset)
	copy(data[ownedMatrixHeader:], identity)
	copy(data[ownedMatrixHeader+len(identity):], generation)
	texts := map[string]int{}
	text := func(value string) int {
		if offset, ok := texts[value]; ok {
			return offset
		}
		offset := len(data) - textOffset
		data = append(data, value...)
		texts[value] = offset
		return offset
	}
	for i, chunk := range source.index.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offset := rowOffset + i*ownedMatrixRow
		put(offset, text(chunk.Path))
		put(offset+8, len(chunk.Path))
		put(offset+16, chunk.StartLine)
		put(offset+24, chunk.EndLine)
		put(offset+32, source.lengths[i])
		for j, value := range source.index.Vectors[i] {
			binary.NativeEndian.PutUint32(data[vectorOffset+(i*dims+j)*4:], math.Float32bits(value))
		}
	}
	position := 0
	for i, term := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offset := termOffset + i*ownedMatrixTerm
		put(offset, text(term))
		put(offset+8, len(term))
		put(offset+16, position)
		put(offset+24, len(source.terms[term]))
		for _, post := range source.terms[term] {
			put(postOffset+position*ownedMatrixPost, post.Document)
			put(postOffset+position*ownedMatrixPost+8, post.Frequency)
			position++
		}
	}
	header(1, len(data))
	return data, nil
}
