package rank

import "github.com/yasyf/cc-context/internal/semsearch"

type corpus interface {
	Len() int
	Chunk(int) semsearch.Chunk
	Vector(int) []float32
}

// AnswerCorpus exposes immutable ranking storage whose owner keeps borrowed result paths alive.
type AnswerCorpus interface {
	corpus
	BM25Scorer
}

type denseCorpus struct {
	chunks  []semsearch.Chunk
	vectors [][]float32
}

func (c denseCorpus) Len() int { return len(c.vectors) }

func (c denseCorpus) Chunk(i int) semsearch.Chunk { return c.chunks[i] }

func (c denseCorpus) Vector(i int) []float32 { return c.vectors[i] }

// RankAnswers shares the exact dense scorer; result paths borrow the source's storage.
func RankAnswers(query string, queryVec []float32, source AnswerCorpus, topK int) []semsearch.Result {
	return rankCorpus(query, queryVec, source, nil, Options{TopK: topK}, source)
}
