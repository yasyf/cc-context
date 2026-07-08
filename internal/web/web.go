package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/backend"
)

// defaultK is the number of search hits returned when a.K is unset (<= 0).
const defaultK = 5

// embedder produces chunk and query embeddings for search. It is a package var
// defaulting to the real uv-subprocess embedder so tests can inject a fake
// without a network or subprocess; it is the least-exported seam that lets Run
// stay a single entry point. It is only read on the search path.
var embedder Embedder = UVEmbedder{}

// fetchPage is the fetch entry point, a package var so tests can drive Run
// through a stubbed cascade without live network. It defaults to Fetch.
var fetchPage = Fetch

// Run fetches, chunks, and serves the page at a.URL for one web op, returning
// shaped-but-uncapped text — the caller passes it through render.Finalize, which
// is the single capping site, so Run never calls render.Cap. It reads a.URL
// (normalized first), a.Query, a.Section, a.Full, a.K (default 5), and a.Force
// (bypass the cache TTL). It panics on a non-web op, an impossible state the
// dispatch layer never produces.
func Run(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	norm, err := NormalizeURL(a.URL)
	if err != nil {
		return "", err
	}
	page, err := acquire(ctx, norm, a.Force)
	if err != nil {
		return "", err
	}
	switch op {
	case backend.OpWebOutline:
		return renderOutline(page), nil
	case backend.OpWebRead:
		return runRead(page, a)
	case backend.OpWebSearch:
		return runSearch(ctx, page, a)
	default:
		panic(fmt.Sprintf("web.Run: non-web op %q", op))
	}
}

// acquire returns the page for norm, from cache when a fresh entry exists and
// force is unset, otherwise through the fetch cascade. A 304 revalidation keeps
// the cached chunks and vectors; a fresh body reuses them only when its content
// hash is unchanged. ErrGone/ErrAuthRequired/ErrBlocked and joined failures
// propagate wrapped for the CLI to map onto exit codes.
func acquire(ctx context.Context, norm string, force bool) (*Page, error) {
	prior, err := Load(norm, EmbedModelID)
	if err != nil {
		return nil, fmt.Errorf("load cached page %q: %w", norm, err)
	}
	if prior != nil && !force && Fresh(prior) {
		return prior, nil
	}

	res, err := fetchPage(ctx, norm, prior)
	if errors.Is(err, ErrNotModified) {
		// The origin confirmed the cached copy is current: keep its chunks and
		// vectors, refresh the fetch time, and re-persist.
		prior.FetchedAt = timeNow()
		if err := Save(prior); err != nil {
			return nil, fmt.Errorf("persist revalidated page %q: %w", norm, err)
		}
		return prior, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", norm, err)
	}

	page, err := buildPage(res, prior, norm)
	if err != nil {
		return nil, fmt.Errorf("build page %q: %w", norm, err)
	}
	if err := Save(page); err != nil {
		return nil, fmt.Errorf("persist page %q: %w", norm, err)
	}
	return page, nil
}

// buildPage turns a fetch result into a Page. HTML-returning tiers run through
// the local extractor; markdown-returning tiers are used directly with the
// tier's title. When the fresh markdown is byte-identical to the prior cache
// entry, its chunks and (costly) vectors are reused rather than recomputed;
// otherwise the page is re-chunked and its vectors dropped for a lazy re-embed.
func buildPage(res FetchResult, prior *Page, norm string) (*Page, error) {
	markdown := res.Markdown
	title := res.Title
	if res.HTML != "" {
		md, extractedTitle, err := Extract(res.HTML, res.FinalURL)
		if err != nil {
			return nil, fmt.Errorf("extract %q: %w", res.FinalURL, err)
		}
		markdown = md
		if title == "" {
			title = extractedTitle
		}
	}

	sha := contentSHA(markdown)
	page := &Page{
		URL:        norm,
		FinalURL:   res.FinalURL,
		Title:      title,
		Tier:       res.Tier,
		FetchedAt:  timeNow(),
		ETag:       res.ETag,
		LastMod:    res.LastMod,
		ContentSHA: sha,
		Markdown:   markdown,
		RawHTML:    res.HTML,
	}
	if prior != nil && prior.ContentSHA == sha {
		page.Sections = prior.Sections
		page.Chunks = prior.Chunks
		page.Vectors = prior.Vectors
		page.EmbedModel = prior.EmbedModel
		return page, nil
	}
	page.Sections, page.Chunks = ChunkPage(markdown)
	return page, nil
}

// contentSHA is the hex SHA-256 of a page's markdown, compared across refetches
// to decide whether chunks and vectors can be reused.
func contentSHA(markdown string) string {
	sum := sha256.Sum256([]byte(markdown))
	return hex.EncodeToString(sum[:])
}

// runRead serves the whole page for --full or a bare read, or a section's
// subtree span with a sibling-navigation footer for --section. A section ref may
// be a bare ID ("2.3") or a cite's "2.3#k7fq" (with an optional leading "§"),
// whose hash re-anchors a drifted section to where its content moved.
func runRead(page *Page, a backend.Args) (string, error) {
	if a.Full || a.Section == "" {
		return page.Markdown, nil
	}
	section, hash, err := splitSectionRef(a.Section)
	if err != nil {
		return "", err
	}
	if hash != "" {
		chunk, err := Resolve(page, section, hash)
		if err != nil {
			return "", err
		}
		section = chunk.Section
	}
	start, end, ok := subtreeSpan(page.Sections, section)
	if !ok {
		return "", fmt.Errorf("section %q not found on %s (run ccx web outline to list sections)", section, page.URL)
	}
	return renderRead(page, section, start, end), nil
}

// runSearch ranks the page's chunks against a.Query with weighted RRF over dense
// (embedding) and lexical (BM25) orderings, returning the top a.K hits. The
// query embedding — plus a first-search embed of every chunk vector — runs
// concurrently with BM25 on this goroutine. Without uv, or on an embed failure,
// search degrades to BM25-only and appends a note rather than failing.
func runSearch(ctx context.Context, page *Page, a backend.Args) (string, error) {
	k := a.K
	if k <= 0 {
		k = defaultK
	}
	texts := chunkTexts(page)

	var (
		chunkVecs [][]float32
		queryVec  []float32
		note      string
		g         *errgroup.Group
	)
	if Supported() {
		var gctx context.Context
		g, gctx = errgroup.WithContext(ctx)
		existing := page.Vectors
		g.Go(func() error {
			vecs, qv, err := embedForSearch(gctx, existing, texts, a.Query)
			if err != nil {
				return err
			}
			chunkVecs, queryVec = vecs, qv
			return nil
		})
	} else {
		note = UnsupportedReason
	}

	lexOrder := newBM25(texts).rank(a.Query)

	var dense []int
	if g != nil {
		if err := g.Wait(); err != nil {
			slog.Warn("web search degraded to BM25-only", "url", page.URL, "err", err)
			note = fmt.Sprintf("hybrid search unavailable (%v); ranked by BM25 alone", err)
		} else {
			if page.Vectors == nil {
				page.Vectors = chunkVecs
				page.EmbedModel = EmbedModelID
				if err := Save(page); err != nil {
					slog.Warn("persist embedded chunk vectors", "url", page.URL, "err", err)
				}
			}
			dense = denseOrder(page.Vectors, queryVec)
		}
	}

	fused := fuse(dense, lexOrder, k)
	scores := fusedScoreSlice(dense, lexOrder)
	return renderSearch(page, a.Query, fused, scores, note), nil
}

// embedForSearch returns the page's chunk vectors and the query vector. When
// existing is nil (a first search) it embeds every chunk text and the query in
// one subprocess call; when the vectors already persist it embeds only the query.
func embedForSearch(ctx context.Context, existing [][]float32, texts []string, query string) (chunkVecs [][]float32, queryVec []float32, err error) {
	if existing != nil {
		qv, err := embedder.Embed(ctx, []string{query})
		if err != nil {
			return nil, nil, err
		}
		return existing, qv[0], nil
	}
	batch := make([]string, 0, len(texts)+1)
	batch = append(batch, texts...)
	batch = append(batch, query)
	vecs, err := embedder.Embed(ctx, batch)
	if err != nil {
		return nil, nil, err
	}
	return vecs[:len(texts)], vecs[len(texts)], nil
}

// chunkTexts is each chunk's source markdown span, the corpus both rankers score.
func chunkTexts(page *Page) []string {
	texts := make([]string, len(page.Chunks))
	for i, c := range page.Chunks {
		texts[i] = page.Markdown[c.Start:c.End]
	}
	return texts
}

// fusedScoreSlice recomputes the per-chunk fusion scores fuse ranks by, indexed
// by chunk, mirroring fuse's own accumulation through the shared addRankScores
// and the package fusion weights so a rendered score never diverges from the
// order fuse returned.
func fusedScoreSlice(dense, lex []int) []float64 {
	fused := make([]float64, len(lex))
	addRankScores(fused, lex, lexWeight)
	if len(dense) > 0 {
		addRankScores(fused, dense, denseWeight)
	}
	return fused
}

// subtreeSpan returns the byte span covering section id and every descendant —
// id's Start through the farthest End in its subtree — and whether id exists.
// Descendants are the sections whose dotted ID is prefixed by "id.".
func subtreeSpan(sections []Section, id string) (int, int, bool) {
	prefix := id + "."
	var start, end int
	found := false
	for _, s := range sections {
		if s.ID != id && !strings.HasPrefix(s.ID, prefix) {
			continue
		}
		if s.ID == id {
			start = s.Start
			found = true
		}
		if s.End > end {
			end = s.End
		}
	}
	if !found {
		return 0, 0, false
	}
	return start, end, true
}

// siblingNav returns the section IDs immediately before and after id among its
// siblings (sections sharing its Parent) in document order; either is empty when
// id sits at an edge.
func siblingNav(sections []Section, id string) (prev, next string) {
	parent := ""
	found := false
	for _, s := range sections {
		if s.ID == id {
			parent = s.Parent
			found = true
			break
		}
	}
	if !found {
		return "", ""
	}
	var sibs []string
	for _, s := range sections {
		if s.Parent == parent {
			sibs = append(sibs, s.ID)
		}
	}
	for i, sid := range sibs {
		if sid != id {
			continue
		}
		if i > 0 {
			prev = sibs[i-1]
		}
		if i+1 < len(sibs) {
			next = sibs[i+1]
		}
	}
	return prev, next
}

// renderOutline renders the page header — title, URL, tier, fetch age, total
// tokens — then one indented line per section carrying its §ID, title, own-span
// token estimate, and chunk count. Section token estimates sum to the page total
// because sections partition the markdown.
func renderOutline(page *Page) string {
	counts := make(map[string]int, len(page.Sections))
	for _, c := range page.Chunks {
		counts[c.Section]++
	}

	var b strings.Builder
	title := page.Title
	if title == "" {
		title = page.URL
	}
	fmt.Fprintf(&b, "# %s\n", title)
	fmt.Fprintf(&b, "%s · %s · fetched %s ago · ~%d tokens\n",
		page.URL, page.Tier, humanizeAge(timeNow().Sub(page.FetchedAt)), estimateTokens(page.Markdown))

	for _, s := range page.Sections {
		indent := s.Level - 1
		if indent < 0 {
			indent = 0
		}
		label := s.Title
		if label == "" {
			label = "(preamble)"
		}
		fmt.Fprintf(&b, "%s§%s  %s  ~%d (%d chunks)\n",
			strings.Repeat("  ", indent), s.ID, label,
			estimateTokens(page.Markdown[s.Start:s.End]), counts[s.ID])
	}
	return b.String()
}

// renderRead renders a section's subtree span followed by a sibling-navigation
// footer, omitting the footer when the section has no siblings to step to.
func renderRead(page *Page, section string, start, end int) string {
	span := page.Markdown[start:end]
	prev, next := siblingNav(page.Sections, section)
	if prev == "" && next == "" {
		return span
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(span, "\n"))
	var parts []string
	if prev != "" {
		parts = append(parts, "§prev "+prev)
	}
	if next != "" {
		parts = append(parts, "§next "+next)
	}
	fmt.Fprintf(&b, "\n\n— %s\n", strings.Join(parts, " | "))
	return b.String()
}

// renderSearch renders the ranked hits: a result count header, then per hit a
// cite line ("<url> §<sec>#<hash>  (score)  breadcrumb") over the chunk text,
// with a trailing note when search ran degraded.
func renderSearch(page *Page, query string, order []int, scores []float64, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d results for %q\n", len(order), query)
	for _, idx := range order {
		c := page.Chunks[idx]
		b.WriteByte('\n')
		fmt.Fprintf(&b, "%s  (%.3f)", FormatCite(page.URL, c.Section, c.Hash), scores[idx])
		if c.Breadcrumb != "" {
			b.WriteString("  " + c.Breadcrumb)
		}
		b.WriteByte('\n')
		b.WriteString(page.Markdown[c.Start:c.End])
		b.WriteByte('\n')
	}
	if note != "" {
		fmt.Fprintf(&b, "\n# %s\n", note)
	}
	return b.String()
}

// humanizeAge renders a coarse single-unit age ("just now", "3m", "5h", "2d")
// for the outline header.
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
