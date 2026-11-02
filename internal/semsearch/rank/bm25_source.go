package rank

// BM25Scorer returns scores in the corpus order used by the dense ranking leg.
type BM25Scorer interface {
	GetScores([]string) []float64
}

// BM25Postings reads an immutable term's postings without materializing its rows.
type BM25Postings interface {
	Len() int
	At(int) BM25Posting
}

// BM25Source supplies corpus statistics and postings from their owning storage.
type BM25Source interface {
	Len() int
	OrderSize() int
	TotalLength() int
	DocumentLength(int) int
	Position(int) int
	Postings(string) BM25Postings
}

type storedPostings []posting

func (p storedPostings) Len() int { return len(p) }

func (p storedPostings) At(i int) BM25Posting {
	return BM25Posting{Document: int(p[i].doc), Frequency: int(p[i].tf)}
}

// Len counts documents included in BM25 corpus statistics.
func (b *BM25) Len() int { return len(b.docLengths) }

// OrderSize includes every original chunk position, including filtered documents.
func (b *BM25) OrderSize() int { return b.nOrder }

// TotalLength is the corpus token count used to derive average document length.
func (b *BM25) TotalLength() int { return b.totalDocLen }

// DocumentLength supplies the original token count before scoring normalization.
func (b *BM25) DocumentLength(i int) int { return b.docLengths[i] }

// Position maps a corpus document to its original chunk slot, or -1 when filtered.
func (b *BM25) Position(i int) int { return b.positions[i] }

// Postings borrows the term's document IDs and occurrence counts without a copy.
func (b *BM25) Postings(term string) BM25Postings {
	id, exists := b.terms[term]
	if !exists {
		return storedPostings(nil)
	}
	return storedPostings(b.postings[id])
}
