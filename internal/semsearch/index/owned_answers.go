package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // Registers the SQLite driver with database/sql.

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/chunk"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

const ownedAnswerFormat = 2

// OwnedAnswerDocument binds an immutable canonical text to its indexed path.
type OwnedAnswerDocument struct {
	ID         string
	Path       string
	TextDigest string
}

// OwnedAnswerText reads canonical Markdown from the still-open source cut.
type OwnedAnswerText interface {
	Text(context.Context, string) (string, error)
}

// OwnedAnswerCorpus carries body-free membership from one validated source cut.
type OwnedAnswerCorpus struct {
	Binding    string
	Generation string
	Documents  []OwnedAnswerDocument
	Source     OwnedAnswerText
}

type ownedAnswerMember struct {
	OwnedAnswerDocument
	key string
}

type ownedAnswerBuild struct {
	member ownedAnswerMember
	chunks []semsearch.Chunk
}

// LoadOwnedAnswers reads numerical metadata and query postings, materializing only new texts.
func LoadOwnedAnswers(ctx context.Context, emb Embedder, corpus OwnedAnswerCorpus, root, query string, chunker Chunker, model string) (*Index, error) {
	identity := fmt.Sprintf("%d %q %q docs %q %d", ownedAnswerFormat, corpus.Binding, model, chunker.ID(), emb.Dims())
	variant := contentHash(identity)
	dir := filepath.Join(root, "ccx", variant)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create owned answer cache: %w", err)
	}
	path := filepath.Join(dir, "index.sqlite")
	members := make([]ownedAnswerMember, len(corpus.Documents))
	for i, document := range corpus.Documents {
		members[i] = ownedAnswerMember{OwnedAnswerDocument: document, key: contentHash(document.Path + "\x00" + document.TextDigest)}
	}
	slices.SortFunc(members, func(a, b ownedAnswerMember) int { return strings.Compare(a.Path, b.Path) })
	idx, found, err := readOwnedAnswers(ctx, path, identity, corpus.Generation, members, query, emb.Dims())
	if err != nil || found {
		return idx, err
	}
	reindexed := 0
	err = cache.WithLock(ctx, dir, "build", func() error {
		var err error
		idx, found, err = readOwnedAnswers(ctx, path, identity, corpus.Generation, members, query, emb.Dims())
		if err != nil || found {
			return err
		}
		reindexed, err = buildOwnedAnswers(ctx, path, identity, emb, corpus, members, chunker)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		idx, found, err = readOwnedAnswers(ctx, path, identity, corpus.Generation, members, query, emb.Dims())
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("owned answer generation was not published")
		}
	}
	idx.Reindexed = reindexed
	return idx, nil
}

func ownedAnswerDB(path, mode string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", mode)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func readOwnedAnswers(ctx context.Context, path, identity, generation string, members []ownedAnswerMember, query string, dims int) (*Index, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	db, err := ownedAnswerDB(path, "ro")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var header string
	if err := tx.QueryRowContext(ctx, "SELECT identity FROM header").Scan(&header); err != nil {
		return nil, false, err
	}
	if header != identity {
		return nil, false, fmt.Errorf("owned answer cache identity mismatch")
	}
	var count int
	err = tx.QueryRowContext(ctx, "SELECT documents FROM cut WHERE generation = ?", generation).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if count != len(members) {
		return nil, false, fmt.Errorf("owned answer membership count mismatch")
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.ordinal,m.content_key,d.path,d.text_digest,d.chunks
		FROM cut_member m LEFT JOIN doc_content d ON d.content_key=m.content_key WHERE m.generation=? ORDER BY m.ordinal`, generation)
	if err != nil {
		return nil, false, err
	}
	i := 0
	expectedChunks := 0
	for rows.Next() {
		var ordinal, count int
		var key, path, digest string
		if err := rows.Scan(&ordinal, &key, &path, &digest, &count); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		if i >= len(members) || ordinal != i || key != members[i].key || path != members[i].Path || digest != members[i].TextDigest || count < 0 {
			_ = rows.Close()
			return nil, false, fmt.Errorf("owned answer membership mismatch")
		}
		expectedChunks += count
		i++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if i != len(members) {
		return nil, false, fmt.Errorf("owned answer membership incomplete")
	}
	idx := &Index{TotalFiles: len(members)}
	var lengths []int
	positions := map[string]map[int]int{}
	rows, err = tx.QueryContext(ctx, `SELECT m.content_key, c.ordinal, d.path, c.start_line, c.end_line, c.doc_length, c.vector
		FROM cut_member m JOIN doc_content d ON d.content_key=m.content_key
		JOIN chunk_meta c ON c.content_key=m.content_key WHERE m.generation=? ORDER BY m.ordinal,c.ordinal`, generation)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var key string
		var ordinal, length int
		var data []byte
		var c semsearch.Chunk
		if err := rows.Scan(&key, &ordinal, &c.Path, &c.StartLine, &c.EndLine, &length, &data); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		if len(data) != dims*4 || length < 0 || c.StartLine < 1 || c.EndLine < c.StartLine || ordinal != len(positions[key]) {
			_ = rows.Close()
			return nil, false, fmt.Errorf("owned answer chunk metadata invalid")
		}
		if positions[key] == nil {
			positions[key] = map[int]int{}
		}
		positions[key][ordinal] = len(idx.Chunks)
		vector := make([]float32, dims)
		for j := range vector {
			vector[j] = math.Float32frombits(binary.LittleEndian.Uint32(data[j*4:]))
		}
		idx.Chunks = append(idx.Chunks, c)
		idx.Vectors = append(idx.Vectors, vector)
		lengths = append(lengths, length)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(idx.Chunks) != expectedChunks {
		return nil, false, fmt.Errorf("owned answer chunks incomplete")
	}
	terms := map[string][]rank.BM25Posting{}
	for _, term := range rank.Tokenize(query) {
		if _, exists := terms[term]; exists {
			continue
		}
		terms[term] = nil
		rows, err = tx.QueryContext(ctx, `SELECT t.content_key,t.ordinal,t.tf FROM chunk_term t
			JOIN cut_member m ON m.content_key=t.content_key WHERE m.generation=? AND t.term=? ORDER BY m.ordinal,t.ordinal`, generation, term)
		if err != nil {
			return nil, false, err
		}
		for rows.Next() {
			var key string
			var ordinal, tf int
			if err := rows.Scan(&key, &ordinal, &tf); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			position, ok := positions[key][ordinal]
			if !ok {
				_ = rows.Close()
				return nil, false, fmt.Errorf("owned answer posting lacks chunk")
			}
			terms[term] = append(terms[term], rank.BM25Posting{Document: position, Frequency: tf})
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, false, err
		}
	}
	idx.BM25, err = rank.BM25FromPostings(lengths, terms)
	if err != nil {
		return nil, false, err
	}
	if len(idx.Chunks) == 0 {
		return nil, false, fmt.Errorf("semsearch: no indexable files in answer corpus")
	}
	return idx, true, tx.Commit()
}

func buildOwnedAnswers(ctx context.Context, path, identity string, emb Embedder, corpus OwnedAnswerCorpus, members []ownedAnswerMember, chunker Chunker) (int, error) {
	_, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !fresh {
		return 0, statErr
	}
	buildPath := path
	if fresh {
		f, err := os.CreateTemp(filepath.Dir(path), ".index-*")
		if err != nil {
			return 0, err
		}
		buildPath = f.Name()
		defer func() {
			_ = os.Remove(buildPath)
			_ = os.Remove(buildPath + "-wal")
			_ = os.Remove(buildPath + "-shm")
		}()
		if err := f.Close(); err != nil {
			return 0, err
		}
	}
	db, err := ownedAnswerDB(buildPath, "rw")
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	if fresh {
		var mode string
		if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return 0, err
		}
		if mode != "wal" {
			return 0, fmt.Errorf("owned answer journal mode is %q", mode)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if fresh {
		_, err = tx.ExecContext(ctx, `CREATE TABLE header(identity TEXT NOT NULL);
			CREATE TABLE doc_content(content_key TEXT PRIMARY KEY,path TEXT NOT NULL,text_digest TEXT NOT NULL,chunks INTEGER NOT NULL) WITHOUT ROWID;
			CREATE TABLE chunk_meta(content_key TEXT NOT NULL,ordinal INTEGER NOT NULL,start_line INTEGER NOT NULL,end_line INTEGER NOT NULL,doc_length INTEGER NOT NULL,vector BLOB NOT NULL,PRIMARY KEY(content_key,ordinal)) WITHOUT ROWID;
			CREATE TABLE chunk_term(term TEXT NOT NULL,content_key TEXT NOT NULL,ordinal INTEGER NOT NULL,tf INTEGER NOT NULL,PRIMARY KEY(term,content_key,ordinal)) WITHOUT ROWID;
			CREATE TABLE cut(generation TEXT PRIMARY KEY,documents INTEGER NOT NULL) WITHOUT ROWID;
			CREATE TABLE cut_member(generation TEXT NOT NULL,ordinal INTEGER NOT NULL,content_key TEXT NOT NULL,PRIMARY KEY(generation,ordinal)) WITHOUT ROWID;
			CREATE INDEX cut_member_content ON cut_member(generation,content_key);`)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO header VALUES (?)", identity); err != nil {
			return 0, err
		}
	}
	reindexed := 0
	var builds []ownedAnswerBuild
	var texts []string
	for i, member := range members {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var path, digest string
		err := tx.QueryRowContext(ctx, "SELECT path,text_digest FROM doc_content WHERE content_key=?", member.key).Scan(&path, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			chunks, err := chunkOwnedAnswer(ctx, corpus.Source, member, chunker)
			if err != nil {
				return 0, err
			}
			builds = append(builds, ownedAnswerBuild{member: member, chunks: chunks})
			for _, c := range chunks {
				texts = append(texts, c.Content)
			}
			reindexed++
		} else if err != nil {
			return 0, err
		} else if path != member.Path || digest != member.TextDigest {
			return 0, fmt.Errorf("owned answer content identity mismatch")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO cut_member VALUES (?,?,?)", corpus.Generation, i, member.key); err != nil {
			return 0, err
		}
	}
	vectors, err := encodeAll(ctx, emb, texts)
	if err != nil {
		return 0, err
	}
	if len(vectors) != len(texts) {
		return 0, fmt.Errorf("owned answer embedding count mismatch")
	}
	position := 0
	for _, build := range builds {
		end := position + len(build.chunks)
		if err := insertOwnedAnswer(ctx, tx, build.member, build.chunks, vectors[position:end], emb.Dims()); err != nil {
			return 0, err
		}
		position = end
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO cut VALUES (?,?)", corpus.Generation, len(members)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if fresh {
		var busy, frames, checkpointed int
		if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil {
			return 0, err
		}
		if busy != 0 || frames != 0 || checkpointed != 0 {
			return 0, fmt.Errorf("owned answer staging checkpoint incomplete: busy=%d frames=%d checkpointed=%d", busy, frames, checkpointed)
		}
	}
	if err := db.Close(); err != nil {
		return 0, err
	}
	if fresh {
		if err := os.Rename(buildPath, path); err != nil {
			return 0, err
		}
	}
	return reindexed, nil
}

func chunkOwnedAnswer(ctx context.Context, source OwnedAnswerText, member ownedAnswerMember, chunker Chunker) ([]semsearch.Chunk, error) {
	text, err := source.Text(ctx, member.ID)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(text))
	if hex.EncodeToString(digest[:]) != member.TextDigest {
		return nil, fmt.Errorf("owned answer canonical text digest mismatch")
	}
	var chunks []semsearch.Chunk
	if len(text) <= maxFileBytes && (len(text) >= emptyFileBytes || strings.TrimSpace(text) != "") {
		text = chunk.DecodeReplace([]byte(text))
		chunks = chunker.ChunkFile(ctx, member.Path, DetectLanguage(member.Path), text)
	}
	return chunks, ctx.Err()
}

func insertOwnedAnswer(ctx context.Context, tx *sql.Tx, member ownedAnswerMember, chunks []semsearch.Chunk, vectors [][]float32, dims int) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO doc_content VALUES (?,?,?,?)", member.key, member.Path, member.TextDigest, len(chunks)); err != nil {
		return err
	}
	for i, c := range chunks {
		if len(vectors[i]) != dims {
			return fmt.Errorf("owned answer embedding dimensions mismatch")
		}
		data := make([]byte, 0, dims*4)
		for _, value := range vectors[i] {
			data = binary.LittleEndian.AppendUint32(data, math.Float32bits(value))
		}
		tokens := rank.Tokenize(rank.EnrichForBM25(c))
		if _, err := tx.ExecContext(ctx, "INSERT INTO chunk_meta VALUES (?,?,?,?,?,?)", member.key, i, c.StartLine, c.EndLine, len(tokens), data); err != nil {
			return err
		}
		counts := map[string]int{}
		for _, token := range tokens {
			counts[token]++
		}
		for term, count := range counts {
			if _, err := tx.ExecContext(ctx, "INSERT INTO chunk_term VALUES (?,?,?,?)", term, member.key, i, count); err != nil {
				return err
			}
		}
	}
	return nil
}
