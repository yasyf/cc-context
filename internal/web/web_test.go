package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
)

// fixtureMarkdown is a small heading tree with a preamble, an H1, two H2s, and
// an H3 under the first H2 — enough to exercise sections, subtree reads, sibling
// navigation, and ranking.
const fixtureMarkdown = "intro paragraph before any heading.\n\n" +
	"# Guide\n\noverview text for the guide.\n\n" +
	"## Errors\n\nHandle errors by wrapping them with %w in Go.\n\n" +
	"### Wrapping\n\nUse fmt.Errorf with the %w verb to wrap.\n\n" +
	"## Install\n\nInstall with homebrew: brew install ccx.\n"

const fixtureURL = "https://example.com/guide"

// fixtureNumbered is a heading tree whose sections print their OWN numbers in the
// heading text ("5.6.7."), deliberately divergent from the chunker's §ids: §1.1
// is printed "5.6.7", §1.2 "5.6.8", and §2.1 reuses printed "5.6.7" so a printed
// number resolves uniquely (5.6.8) or ambiguously (5.6.7).
const fixtureNumbered = "Preamble line before any heading.\n\n" +
	"# Reference\n\nReference intro.\n\n" +
	"## 5.6.7. Date/Time Formats\n\nDate and time formatting rules go here.\n\n" +
	"## 5.6.8. Number Formats\n\nNumber formatting rules go here.\n\n" +
	"# Appendix\n\nAppendix intro paragraph.\n\n" +
	"## 5.6.7. Legacy Formats\n\nLegacy duplicate of the 5.6.7 printed number.\n"

// fixtureLong is a single long section whose lines carry unique markers, so a
// budget-capped read pages: page one keeps alpha-marker, a later offset reaches
// bravo-marker.
const fixtureLong = "# Log\n\n" +
	"alpha-marker aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
	"bravo-marker bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
	"charlie ccccccccccccccccccccccccccccccccccccccccc\n" +
	"delta ddddddddddddddddddddddddddddddddddddddddddd\n" +
	"echo eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\n" +
	"zeta-marker zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\n"

// fixtureFlat is a headingless page: ChunkPage yields the single preamble
// section, the degenerate case renderOutline hints about.
const fixtureFlat = "just a wall of text with no headings at all.\n\nmore text here.\n"

// fixtureLinkedTitle carries an RFC-9110-style heading whose title wraps its
// printed number in an inline link and brackets its text, so a printed-number
// read exercises title normalization end to end.
const fixtureLinkedTitle = "# Reference\n\n" +
	"## [5.6.7.](#section-5.6.7) [Date/Time Formats]\n\n" +
	"Date and time formatting rules go here.\n"

// webCtx returns the context a test drives this package under: a cache root of
// its own and every tier key pinned empty, so neither a key exported on the
// developer's machine nor a concurrent test's environment decides which lanes
// exist. Later entries win, so env overrides any of them.
func webCtx(t *testing.T, env ...string) context.Context {
	t.Helper()
	return render.WithEnv(t.Context(), append([]string{
		"CLAUDE_PLUGIN_DATA=" + t.TempDir(),
		envJinaKey + "=",
		envExaKey + "=",
		envFirecrawlKey + "=",
		envBrowserbaseKey + "=",
	}, env...)...)
}

// allKeysSet enables every hosted tier, for a test whose refusal must fire
// before any of them runs.
var allKeysSet = []string{
	envJinaKey + "=jina-key",
	envExaKey + "=exa-key",
	envFirecrawlKey + "=fc-key",
	envBrowserbaseKey + "=bb-key",
}

// fetchFunc mirrors the signature of Fetch (and the runner's fetch seam).
type fetchFunc = func(context.Context, string, *Page) (FetchResult, error)

// fixtureNow is the instant every Run test's store clock is pinned to, so a
// page's rendered age and its freshness against the TTL are deterministic.
var fixtureNow = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

// testRunner returns a runner with render escalation off and its store clock
// pinned to fixtureNow. Escalation stays off because most fixtures here are
// sub-floor and would trip thinSignature into a real render subprocess; a test
// exercising it assigns r.render, as it assigns r.fetch and r.embedder for the
// other seams it drives.
func testRunner() *runner {
	s := newStore()
	s.now = func() time.Time { return fixtureNow }
	return &runner{store: s}
}

// fixedEmbedder is the runner's embedder provider for a test driving search
// through a fake engine.
func fixedEmbedder(e Embedder) func(context.Context) (Embedder, error) {
	return func(context.Context) (Embedder, error) { return e, nil }
}

// markdownFetch stubs the cascade to return fixed markdown as a jina result and
// counts how many times it is called.
func markdownFetch(md, title string, calls *atomic.Int32) fetchFunc {
	return func(_ context.Context, url string, _ *Page) (FetchResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		return FetchResult{Tier: TierJina, FinalURL: url, Title: title, Markdown: md}, nil
	}
}

// fakeEmbedder returns deterministic 3-dim unit vectors keyed on coarse topic
// so a query aligns densely with the matching chunk, and records call count.
type fakeEmbedder struct {
	calls atomic.Int32
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		lt := strings.ToLower(t)
		switch {
		case strings.Contains(lt, "error"):
			out[i] = []float32{1, 0, 0}
		case strings.Contains(lt, "install"):
			out[i] = []float32{0, 1, 0}
		default:
			out[i] = []float32{0, 0, 1}
		}
	}
	return out, nil
}

// indexedPage builds a chunked page for md, stamped at the current test clock.
func indexedPage(url, md string) *Page {
	sections, chunks := ChunkPage(md)
	return &Page{
		URL:        url,
		FinalURL:   url,
		Title:      "Guide",
		Tier:       TierJina,
		FetchedAt:  fixtureNow,
		ContentSHA: contentSHA(md),
		Markdown:   md,
		Sections:   sections,
		Chunks:     chunks,
	}
}

func attachVectors(p *Page) *Page {
	p.Vectors = make([][]float32, len(p.Chunks))
	for i := range p.Vectors {
		p.Vectors[i] = []float32{1, 0, 0}
	}
	p.EmbedModel = EmbedModelID
	return p
}

// firstCiteSection extracts the section ID of the first cite line in search
// output, e.g. "https://… §1.1#k7fq  (0.031)  Guide > Errors" -> "1.1".
func firstCiteSection(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, citeSep)
		if i < 0 {
			continue
		}
		rest := line[i+len(citeSep):]
		hashIdx := strings.IndexByte(rest, '#')
		if hashIdx < 0 {
			continue
		}
		return rest[:hashIdx]
	}
	t.Fatalf("no cite line in output:\n%s", out)
	return ""
}

func TestRunOutlineThenReadRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	var calls atomic.Int32
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", &calls)

	outline, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("outline Run: %v", err)
	}
	for _, want := range []string{"# Guide", fixtureURL, "jina", "§0  (preamble)", "§1  Guide", "§1.1  Errors", "§1.1.1  Wrapping", "§1.2  Install", "chunks)"} {
		if !strings.Contains(outline, want) {
			t.Errorf("outline missing %q:\n%s", want, outline)
		}
	}

	// The §ID echoed from the outline reads back that section's subtree.
	read, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1.1"})
	if err != nil {
		t.Fatalf("read Run: %v", err)
	}
	for _, want := range []string{"## Errors", "### Wrapping", "— §next 1.2"} {
		if !strings.Contains(read, want) {
			t.Errorf("read of §1.1 missing %q:\n%s", want, read)
		}
	}
	if strings.Contains(read, "## Install") {
		t.Errorf("read of §1.1 leaked the sibling §1.2 Install section:\n%s", read)
	}

	// The second Run served from cache: no second fetch.
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1 (read must hit the fresh cache)", calls.Load())
	}
}

func TestRunReadFullAndBare(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args backend.Args
	}{
		{"full", backend.Args{URL: fixtureURL, Full: true}},
		{"bare", backend.Args{URL: fixtureURL}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := webCtx(t)
			r := testRunner()
			r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

			out, err := r.run(ctx, backend.OpWebRead, tt.args)
			if err != nil {
				t.Fatalf("read Run: %v", err)
			}
			if out != fixtureMarkdown {
				t.Errorf("read = %q, want the whole page", out)
			}
		})
	}
}

func TestRunReadSiblingFooter(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	read, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1.2"})
	if err != nil {
		t.Fatalf("read Run: %v", err)
	}
	if !strings.Contains(read, "— §prev 1.1") {
		t.Errorf("read of §1.2 missing prev-sibling footer:\n%s", read)
	}
	if strings.Contains(read, "§next") {
		t.Errorf("read of last sibling §1.2 should have no next footer:\n%s", read)
	}
}

func TestRunReadUnknownSection(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	_, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "9.9"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("read of unknown section: err = %v, want a not-found error", err)
	}
}

func TestRunReadPrintedNumberResolves(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureNumbered, "Reference", nil)

	// The printed number 5.6.8 is unique: it resolves to §1.2 with a mapping note.
	out, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "5.6.8"})
	if err != nil {
		t.Fatalf("read of printed number 5.6.8: %v", err)
	}
	if want := "# printed number \"5.6.8\" resolved to §1.2 (5.6.8. Number Formats)\n"; !strings.HasPrefix(out, want) {
		t.Errorf("resolve note missing or not prepended:\n%s", out)
	}
	if !strings.Contains(out, "Number formatting rules go here.") {
		t.Errorf("resolved read missing §1.2 body:\n%s", out)
	}
	if strings.Contains(out, "Date/Time Formats") || strings.Contains(out, "Legacy") {
		t.Errorf("resolved read leaked another printed-5.6.7 section:\n%s", out)
	}
}

// TestRunReadPrintedNumberNormalizesNote proves the resolve note shows the printed
// number's title with its markdown markup stripped (F7), not the raw heading text.
func TestRunReadPrintedNumberNormalizesNote(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureLinkedTitle, "Reference", nil)

	for _, section := range []string{"5.6.7", "5.6.7."} {
		out, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: section})
		if err != nil {
			t.Fatalf("read printed number %q: %v", section, err)
		}
		want := fmt.Sprintf("# printed number %q resolved to §1.1 (5.6.7. Date/Time Formats)\n", section)
		if !strings.HasPrefix(out, want) {
			t.Errorf("note not normalized, want prefix %q:\n%s", want, out)
		}
	}
}

func TestRunReadPrintedNumberAmbiguous(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureNumbered, "Reference", nil)

	_, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "5.6.7"})
	if err == nil {
		t.Fatal("read of ambiguous printed number: want an error")
	}
	for _, want := range []string{
		"printed number \"5.6.7\" matches multiple sections:",
		"§1.1 (5.6.7. Date/Time Formats)",
		"§2.1 (5.6.7. Legacy Formats)",
		"pick one by its §id",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguous error = %q, want it to contain %q", err, want)
		}
	}
}

func TestRunReadNotFoundHints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		section    string
		wantNear   string // substring that must appear
		wantAbsent string // substring that must not appear
	}{
		{"id-shaped offers nearest", "1.9", "; nearest surviving section is §1", ""},
		{"text input routes to search", "Date/Time", "ccx web search to find a heading by text", "nearest surviving section"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := webCtx(t)
			r := testRunner()
			r.fetch = markdownFetch(fixtureNumbered, "Reference", nil)

			_, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: tt.section})
			if err == nil {
				t.Fatalf("read of %q: want a not-found error", tt.section)
			}
			msg := err.Error()
			if !strings.Contains(msg, fmt.Sprintf("section %q not found on", tt.section)) {
				t.Errorf("error = %q, want the not-found prefix for %q", msg, tt.section)
			}
			if !strings.Contains(msg, tt.wantNear) {
				t.Errorf("error = %q, want it to contain %q", msg, tt.wantNear)
			}
			if tt.wantAbsent != "" && strings.Contains(msg, tt.wantAbsent) {
				t.Errorf("error = %q, must not contain %q", msg, tt.wantAbsent)
			}
		})
	}
}

var nextOffsetRe = regexp.MustCompile(`--offset (\d+) to continue`)

// A rendered page carries decorations that ride outside the budget window: the
// printed-number resolve note (prefix), the continuation footer (suffix), and the
// sibling-navigation footer (suffix, appended after the continuation footer).
// withNav trims the continuation footer's trailing newline when it appends nav, so
// that newline is optional in contFooterRe.
var (
	noteRe       = regexp.MustCompile(`\A# printed number "[^"]*" resolved to §[^\n]*\n`)
	contFooterRe = regexp.MustCompile(`\n… \+\d+ lines, ~\d+ tokens omitted — re-run with --offset \d+ to continue, or a larger --budget\n?\z`)
	navRe        = regexp.MustCompile(`\n\n— [^\n]*\n\z`)
)

// servedContent recovers a page's served stride-window bytes by stripping those
// decorations, each an exact anchored match so a real content-loss bug can never be
// masked. Nav is stripped before the footer because it is the outermost suffix.
func servedContent(page string) string {
	page = noteRe.ReplaceAllString(page, "")
	page = navRe.ReplaceAllString(page, "")
	page = contFooterRe.ReplaceAllString(page, "")
	return page
}

// pageThrough reads args, then follows each continuation footer's --offset until a
// page has no footer, returning every page's output. Under fixed-stride paging a
// followed footer always names a valid page start, so any read error — including a
// past-end error — is a failure, as is a footer that fails to advance (infinite
// loop) or a run that never terminates within maxPages.
func pageThrough(ctx context.Context, t *testing.T, r *runner, args backend.Args, maxPages int) []string {
	t.Helper()
	var pages []string
	offset := args.Offset
	for i := 0; i < maxPages; i++ {
		a := args
		a.Offset = offset
		out, err := r.run(ctx, backend.OpWebRead, a)
		if err != nil {
			t.Fatalf("page %d at offset %d: %v", i, offset, err)
		}
		pages = append(pages, out)
		m := nextOffsetRe.FindStringSubmatch(out)
		if m == nil {
			return pages
		}
		n, _ := strconv.Atoi(m[1])
		if n <= offset {
			t.Fatalf("footer offset %d did not advance past %d (infinite loop):\n%s", n, offset, out)
		}
		offset = n
	}
	t.Fatalf("paging did not terminate within %d pages", maxPages)
	return nil
}

func TestRunReadOffsetPaging(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureLong, "Log", nil)

	const budget = 15 // limit 60 chars: page one keeps the header + first body line only

	page1, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1", Budget: budget})
	if err != nil {
		t.Fatalf("page one read: %v", err)
	}
	if !strings.Contains(page1, "alpha-marker") {
		t.Errorf("page one missing the first-line marker:\n%s", page1)
	}
	if strings.Contains(page1, "bravo-marker") {
		t.Errorf("page one leaked the second-line marker past the cap:\n%s", page1)
	}
	m := nextOffsetRe.FindStringSubmatch(page1)
	if m == nil {
		t.Fatalf("page one has no continuation footer naming the next offset:\n%s", page1)
	}
	next, _ := strconv.Atoi(m[1])
	if next <= 0 {
		t.Fatalf("page one advertised a non-advancing offset %d:\n%s", next, page1)
	}

	page2, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1", Budget: budget, Offset: next})
	if err != nil {
		t.Fatalf("page two read at offset %d: %v", next, err)
	}
	if strings.Contains(page2, "alpha-marker") {
		t.Errorf("page two at offset %d re-served the first-line marker (overlap):\n%s", next, page2)
	}

	// Following the footers to the end reconstructs the §1 subtree span exactly:
	// fixed-stride pages join byte-for-byte, so no content is dropped or repeated.
	sections, _ := ChunkPage(fixtureLong)
	start, end, _ := subtreeSpan(sections, "1")
	pages := pageThrough(ctx, t, r, backend.Args{URL: fixtureURL, Section: "1", Budget: budget}, 20)
	var sb strings.Builder
	for _, p := range pages {
		sb.WriteString(servedContent(p))
	}
	if got := sb.String(); got != fixtureLong[start:end] {
		t.Errorf("paged §1 read did not reconstruct its span:\n got = %q\nwant = %q", got, fixtureLong[start:end])
	}
}

// siblingPagedMarkdown is a two-H2 page whose §1.1 (Alpha) has a sibling §1.2
// (Beta), so reading §1.1 carries the sibling nav on every page — the case that
// exposes the reconstruction blind spot when nav is appended after the footer.
const siblingPagedMarkdown = "# Guide\n\n" +
	"## Alpha\n\n" +
	"aaaa-marker aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
	"bbbb-marker bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
	"cccc-marker cccccccccccccccccccccccccccccccccccc\n\n" +
	"## Beta\n\nBeta body paragraph.\n"

// TestRunReadPagingReconstructs is the primary no-loss guarantee: following the
// continuation footers from offset 0, the concatenation of every page's served
// content (decorations stripped) equals the fetched span byte-for-byte. Fixed-stride
// pages may start or end mid-line or on a snapped rune boundary, but they never
// drop, split away, or repeat a byte — and page one never advertises --offset 0. A
// section read with siblings carries nav on every page; withNav trims the trailing
// newline off the final page's content, an exact allowance the assertion accounts for.
func TestRunReadPagingReconstructs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		markdown string
		section  string // "" reads the whole page (no nav); else a §id with siblings
		budget   int
	}{
		// The three review repros the fixed-stride design must serve without loss.
		{"repro a short lines lose no line", "ab\ncdef\nghij\n", "", 1},
		{"repro b long first line loses no tail", "abcdefghij\nZ\n", "", 1},
		{"repro c many sub-token lines stay monotonic", "a\nbb\nccc\ndddd\neeeee\n", "", 1},
		// Cap-aligned lines, a page whose window ends exactly at span end, two
		// multi-byte runes straddling a stride boundary, and a realistic page.
		{"aligned short lines", "aaaa\nbbbb\ncccc\ndddd\n", "", 1},
		{"page ends exactly at span end", "abcdefgh", "", 1},
		{"emoji straddles a stride boundary", "ab😀cd\nef\n", "", 1},
		{"cjk straddles a stride boundary", "a中文b\ncd\n", "", 1},
		{"realistic multi-paragraph", fixtureMarkdown, "", 8},
		// A paged section WITH siblings: nav rides on every page, after the footer.
		{"paged section with siblings keeps nav on every page", siblingPagedMarkdown, "1.1", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := webCtx(t)
			r := testRunner()
			r.fetch = markdownFetch(tt.markdown, "Doc", nil)

			args := backend.Args{URL: fixtureURL, Budget: tt.budget}
			want := tt.markdown
			if tt.section == "" {
				args.Full = true
			} else {
				args.Section = tt.section
				sections, _ := ChunkPage(tt.markdown)
				start, end, ok := subtreeSpan(sections, tt.section)
				if !ok {
					t.Fatalf("fixture has no section %q", tt.section)
				}
				want = tt.markdown[start:end]
			}

			pages := pageThrough(ctx, t, r, args, 200)
			if len(pages) == 0 {
				t.Fatal("no pages served")
			}
			if m := nextOffsetRe.FindStringSubmatch(pages[0]); m != nil {
				if n, _ := strconv.Atoi(m[1]); n <= 0 {
					t.Fatalf("page one advertised --offset %d (would loop forever):\n%s", n, pages[0])
				}
			}
			var sb strings.Builder
			for _, p := range pages {
				sb.WriteString(servedContent(p))
			}
			got := sb.String()

			if !navRe.MatchString(pages[len(pages)-1]) {
				if got != want {
					t.Errorf("reconstruction mismatch across %d pages:\n got = %q\nwant = %q", len(pages), got, want)
				}
				return
			}
			// withNav trims the trailing newline(s) off the final page's content, so a
			// nav-bearing read reconstructs the span minus exactly its trailing
			// newlines — never a dropped content byte.
			if trimmed := strings.TrimRight(want, "\n"); got != trimmed {
				t.Errorf("reconstruction mismatch across %d pages:\n got = %q\nwant = %q", len(pages), got, trimmed)
			}
			if lost := len(want) - len(got); lost < 1 || lost > 2 {
				t.Errorf("nav trim dropped %d bytes at the final page; want exactly the 1-2 trailing newlines", lost)
			}
		})
	}
}

// TestRunReadFullOffsetPaging proves --full pages end to end: the offset is applied
// to the whole page (F2), not ignored, and a follow-the-footer loop serves every
// section's marker.
func TestRunReadFullOffsetPaging(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureLong, "Log", nil)

	const budget = 15
	page1, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Full: true, Budget: budget})
	if err != nil {
		t.Fatalf("full page one: %v", err)
	}
	if strings.Contains(page1, "zeta-marker") {
		t.Errorf("full page one leaked the last-line marker past the cap:\n%s", page1)
	}
	// The offset applies to the whole page (F2), and following the footers
	// reconstructs every byte of it.
	pages := pageThrough(ctx, t, r, backend.Args{URL: fixtureURL, Full: true, Budget: budget}, 20)
	var sb strings.Builder
	for _, p := range pages {
		sb.WriteString(servedContent(p))
	}
	if got := sb.String(); got != fixtureLong {
		t.Errorf("full paged read did not reconstruct the page:\n got = %q\nwant = %q", got, fixtureLong)
	}
}

func TestRunReadOffsetGuards(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    backend.Args
		wantErr string
	}{
		{"negative section", backend.Args{URL: fixtureURL, Section: "1", Offset: -1}, "--offset must be non-negative"},
		{"negative full", backend.Args{URL: fixtureURL, Full: true, Offset: -5}, "--offset must be non-negative"},
		{"section past end", backend.Args{URL: fixtureURL, Section: "1", Offset: 100000}, "past the end of section §1"},
		{"maxint64 does not overflow", backend.Args{URL: fixtureURL, Section: "1", Offset: math.MaxInt64}, "past the end of section §1"},
		{"maxint does not overflow", backend.Args{URL: fixtureURL, Section: "1", Offset: math.MaxInt}, "past the end of section §1"},
		{"full past end names the page", backend.Args{URL: fixtureURL, Full: true, Offset: math.MaxInt64}, "past the end of the page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := webCtx(t)
			r := testRunner()
			r.fetch = markdownFetch(fixtureLong, "Log", nil)

			_, err := r.run(ctx, backend.OpWebRead, tt.args)
			if err == nil {
				t.Fatalf("offset %d: want an error", tt.args.Offset)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestRunReadNavFitsBudgetNoOffset proves the sibling-nav footer rides outside the
// budget cap (F6): a section whose content fits the budget advertises no
// continuation offset even when content plus nav would overflow it.
func TestRunReadNavFitsBudgetNoOffset(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	// Budget exactly covers §1.1's content bytes; the nav footer's bytes must not
	// tip it into a spurious continuation footer.
	sections, _ := ChunkPage(fixtureMarkdown)
	start, end, _ := subtreeSpan(sections, "1.1")
	budget := (end - start + charsPerToken - 1) / charsPerToken

	out, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1.1", Budget: budget})
	if err != nil {
		t.Fatalf("read §1.1: %v", err)
	}
	if !strings.Contains(out, "— §next 1.2") {
		t.Errorf("read of §1.1 missing its sibling nav footer:\n%s", out)
	}
	if strings.Contains(out, "to continue") {
		t.Errorf("content fit the budget but a continuation offset was advertised:\n%s", out)
	}
	if !strings.Contains(out, "### Wrapping") {
		t.Errorf("read of §1.1 dropped body that fit the budget:\n%s", out)
	}
}

// TestRunReadResolvedNotePagesWithoutSkip proves the resolve-note rides outside the
// budget cap (N1): a printed-number-resolved page-1 footer's next offset lands so
// page two serves the marker that sat just past the page-1 boundary — no skip.
func TestRunReadResolvedNotePagesWithoutSkip(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()

	md := "# Reference\n\n" +
		"## 5.6.8. Number Formats\n\n" +
		"nzero xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n" +
		"none1 yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy\n" +
		"ntwo2 zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\n"
	r.fetch = markdownFetch(md, "Reference", nil)

	const notePrefix = "# printed number \"5.6.8\" resolved to §1.1 (5.6.8. Number Formats)\n"
	page1, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "5.6.8", Budget: 20})
	if err != nil {
		t.Fatalf("resolved page one: %v", err)
	}
	if !strings.HasPrefix(page1, notePrefix) {
		t.Fatalf("page one missing the resolve note prefix:\n%s", page1)
	}

	// The resolve note rides outside the stride window on every page, so stripping
	// it and the footer reconstructs the §1.1 subtree span with no skipped byte.
	sections, _ := ChunkPage(md)
	start, end, _ := subtreeSpan(sections, "1.1")
	pages := pageThrough(ctx, t, r, backend.Args{URL: fixtureURL, Section: "5.6.8", Budget: 20}, 20)
	var sb strings.Builder
	for _, p := range pages {
		sb.WriteString(servedContent(p))
	}
	if got := sb.String(); got != md[start:end] {
		t.Errorf("note-bearing paged read did not reconstruct §1.1:\n got = %q\nwant = %q", got, md[start:end])
	}
}

// TestRunOutlineAgeMeasuredFromTheStoreClock pins the header's age to the store
// clock at both ends: the fetch stamps FetchedAt from it, and the outline three
// hours later measures against it rather than the wall clock.
func TestRunOutlineAgeMeasuredFromTheStoreClock(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	r.store.now = func() time.Time { return fixtureNow.Add(3 * time.Hour) }

	out, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "fetched 3h ago") {
		t.Errorf("outline header age is not measured from the store clock:\n%s", out)
	}
}

func TestRunOutlineDegenerateHint(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = func(_ context.Context, url string, _ *Page) (FetchResult, error) {
		md := fixtureMarkdown
		if strings.Contains(url, "flat") {
			md = fixtureFlat
		}
		return FetchResult{Tier: TierJina, FinalURL: url, Markdown: md}, nil
	}

	flat, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: "https://example.com/flat"})
	if err != nil {
		t.Fatalf("flat outline: %v", err)
	}
	for _, want := range []string{"This page has no heading structure to navigate", "ccx web read --section <id> --offset N"} {
		if !strings.Contains(flat, want) {
			t.Errorf("degenerate outline missing %q:\n%s", want, flat)
		}
	}

	rich, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("rich outline: %v", err)
	}
	if strings.Contains(rich, "no heading structure") {
		t.Errorf("multi-section outline wrongly showed the degenerate hint:\n%s", rich)
	}
}

func TestRunSearchHybrid(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	fake := &fakeEmbedder{}
	r.embedder = fixedEmbedder(fake)
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	out, err := r.run(ctx, backend.OpWebSearch, backend.Args{URL: fixtureURL, Query: "how do I handle errors", K: 3})
	if err != nil {
		t.Fatalf("search run: %v", err)
	}
	if !strings.HasPrefix(out, "# 3 results for") {
		t.Errorf("search header wrong:\n%s", out)
	}
	if got := firstCiteSection(t, out); got != "1.1" {
		t.Errorf("top hit section = %q, want %q (Errors):\n%s", got, "1.1", out)
	}
	if !strings.Contains(out, fixtureURL+citeSep) {
		t.Errorf("search hits missing cite lines:\n%s", out)
	}
	if fake.calls.Load() == 0 {
		t.Error("hybrid search never called the embedder")
	}
	if strings.Contains(out, UnsupportedReason) {
		t.Errorf("hybrid search wrongly printed the BM25-only note:\n%s", out)
	}

	// A first search persists the chunk vectors for reuse.
	page, err := r.store.load(ctx, fixtureURL, EmbedModelID)
	if err != nil || page == nil {
		t.Fatalf("load after search: page=%v err=%v", page, err)
	}
	if len(page.Vectors) != len(page.Chunks) || page.EmbedModel != EmbedModelID {
		t.Errorf("vectors not persisted: %d vectors for %d chunks, model %q", len(page.Vectors), len(page.Chunks), page.EmbedModel)
	}
}

func TestRunSearchBM25OnlyWhenUnsupported(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.embedder = func(context.Context) (Embedder, error) {
		return nil, embed.ErrWeightsUnavailable
	}
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	out, err := r.run(ctx, backend.OpWebSearch, backend.Args{URL: fixtureURL, Query: "handle errors"})
	if err != nil {
		t.Fatalf("search run: %v", err)
	}
	if !strings.Contains(out, UnsupportedReason) {
		t.Errorf("BM25-only search missing the degradation note:\n%s", out)
	}
	if got := firstCiteSection(t, out); got != "1.1" {
		t.Errorf("BM25-only top hit = %q, want %q:\n%s", got, "1.1", out)
	}
}

func TestRunSearchDegradesOnEmbedError(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.embedder = fixedEmbedder(&fakeEmbedder{err: fmt.Errorf("driver blew up")})
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	out, err := r.run(ctx, backend.OpWebSearch, backend.Args{URL: fixtureURL, Query: "handle errors"})
	if err != nil {
		t.Fatalf("search must not fail on embed error: %v", err)
	}
	if !strings.Contains(out, "BM25") {
		t.Errorf("embed-failure search missing the BM25 fallback note:\n%s", out)
	}
	if got := firstCiteSection(t, out); got != "1.1" {
		t.Errorf("degraded top hit = %q, want %q:\n%s", got, "1.1", out)
	}
}

func TestRunForceRefetch(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	var calls atomic.Int32
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", &calls)

	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Without Force the fresh cache serves; with Force the cascade runs again.
	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("cached Run: %v", err)
	}
	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL, Force: true}); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2 (initial + forced; the middle Run is cached)", calls.Load())
	}
}

func TestRunNotModifiedPreservesVectors(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()

	// A stale cached page carrying vectors; the refetch will 304.
	prior := attachVectors(indexedPage(fixtureURL, fixtureMarkdown))
	prior.FetchedAt = fixtureNow.Add(-48 * time.Hour)
	if err := r.store.save(ctx, prior); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	wantVecs := len(prior.Vectors)

	r.fetch = func(_ context.Context, _ string, p *Page) (FetchResult, error) {
		if p == nil {
			t.Error("revalidation fetch got a nil prior")
		}
		return FetchResult{}, fmt.Errorf("http: %w", ErrNotModified)
	}

	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := r.store.load(ctx, fixtureURL, EmbedModelID)
	if err != nil || got == nil {
		t.Fatalf("load: page=%v err=%v", got, err)
	}
	if len(got.Vectors) != wantVecs {
		t.Errorf("Vectors = %d, want %d preserved across 304", len(got.Vectors), wantVecs)
	}
	if !got.FetchedAt.Equal(fixtureNow) {
		t.Errorf("FetchedAt = %v, want bumped to %v", got.FetchedAt, fixtureNow)
	}
}

func TestRunContentUnchangedPreservesVectors(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()

	prior := attachVectors(indexedPage(fixtureURL, fixtureMarkdown))
	prior.FetchedAt = fixtureNow.Add(-48 * time.Hour)
	if err := r.store.save(ctx, prior); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	wantVecs := len(prior.Vectors)

	// A hosted-tier refetch returning byte-identical markdown: same ContentSHA.
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)

	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL, Force: true}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := r.store.load(ctx, fixtureURL, EmbedModelID)
	if err != nil || got == nil {
		t.Fatalf("load: page=%v err=%v", got, err)
	}
	if len(got.Vectors) != wantVecs {
		t.Errorf("Vectors = %d, want %d preserved when ContentSHA is unchanged", len(got.Vectors), wantVecs)
	}
}

func TestRunContentChangedDropsVectors(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()

	prior := attachVectors(indexedPage(fixtureURL, fixtureMarkdown))
	prior.FetchedAt = fixtureNow.Add(-48 * time.Hour)
	if err := r.store.save(ctx, prior); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	changed := fixtureMarkdown + "\n## New\n\nfresh content.\n"
	r.fetch = markdownFetch(changed, "Guide", nil)

	if _, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL, Force: true}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := r.store.load(ctx, fixtureURL, "")
	if err != nil || got == nil {
		t.Fatalf("load: page=%v err=%v", got, err)
	}
	if len(got.Vectors) != 0 {
		t.Errorf("Vectors = %d, want 0 dropped on content change", len(got.Vectors))
	}
	if got.ContentSHA != contentSHA(changed) {
		t.Errorf("ContentSHA not updated to the changed body")
	}
}

func TestRunGonePropagates(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = func(_ context.Context, _ string, _ *Page) (FetchResult, error) {
		return FetchResult{}, fmt.Errorf("jina: %w", ErrGone)
	}

	_, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err == nil {
		t.Fatal("run: want ErrGone to propagate")
	}
	// The sentinel must survive the wrap so the CLI can map it onto an exit code.
	if !errors.Is(err, ErrGone) {
		t.Errorf("err = %v, want it to wrap ErrGone", err)
	}
}

func TestRunPanicsOnNonWebOp(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)
	defer func() {
		if recover() == nil {
			t.Error("run did not panic on a non-web op")
		}
	}()
	_, _ = r.run(ctx, backend.OpSearch, backend.Args{URL: fixtureURL})
}

func TestRunThinNoLaneServesNoteAllOps(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t, "PATH=")
	r := testRunner()
	r.fetch = markdownFetch("loading", "App", nil)
	r.render = func(context.Context, string) (FetchResult, bool, error) {
		return FetchResult{}, false, errors.New("no render lane available")
	}

	const wantNote = "set JINA_API_KEY"
	for _, op := range []backend.Op{backend.OpWebOutline, backend.OpWebRead, backend.OpWebSearch} {
		out, err := r.run(ctx, op, backend.Args{URL: fixtureURL, Query: "x"})
		if err != nil {
			t.Fatalf("run %v: %v", op, err)
		}
		if !strings.Contains(out, wantNote) {
			t.Errorf("op %v output missing the thin note %q:\n%s", op, wantNote, out)
		}
	}
	page, err := r.store.load(ctx, fixtureURL, EmbedModelID)
	if err != nil || page == nil {
		t.Fatalf("load: page=%v err=%v", page, err)
	}
	if !page.Thin {
		t.Error("persisted page.Thin = false, want true")
	}
}

func TestRunThinEscalatesServesRendered(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t, "PATH=")
	r := testRunner()
	r.fetch = markdownFetch("loading", "App", nil)
	rendered := "# Rendered\n\n" + strings.Repeat("real rendered prose here. ", 20)
	r.render = func(context.Context, string) (FetchResult, bool, error) {
		return FetchResult{Tier: TierJinaRender, FinalURL: fixtureURL, Title: "Rendered", Markdown: rendered}, false, nil
	}

	out, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "jina-render") || !strings.Contains(out, "# Rendered") {
		t.Errorf("outline did not serve the rendered result:\n%s", out)
	}
	if strings.Contains(out, "static content") {
		t.Errorf("a non-thin rendered page carried a thin note:\n%s", out)
	}
	page, _ := r.store.load(ctx, fixtureURL, EmbedModelID)
	if page == nil || page.Thin {
		t.Errorf("page.Thin = %v, want false (rendered result is not thin)", page != nil && page.Thin)
	}
}

func TestRunThinStillThinKeepsLargest(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t, envJinaKey+"=jina-key", "PATH=")
	r := testRunner()
	r.fetch = markdownFetch("hi", "App", nil)
	larger := "still thin but a good deal larger than the original body"
	r.render = func(context.Context, string) (FetchResult, bool, error) {
		return FetchResult{Tier: TierJinaRender, FinalURL: fixtureURL, Title: "App", Markdown: larger}, true, nil
	}

	out, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Full: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(out, larger) {
		t.Errorf("read did not serve the larger thin body:\n%s", out)
	}
	if !strings.Contains(out, "may genuinely have little static content") {
		t.Errorf("thin read missing the lane-available note:\n%s", out)
	}
	page, _ := r.store.load(ctx, fixtureURL, EmbedModelID)
	if page == nil || !page.Thin {
		t.Errorf("page.Thin = %v, want true", page != nil && page.Thin)
	}
}

func TestRunThinNoteSurvivesCacheHit(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t, "PATH=")
	r := testRunner()
	var calls atomic.Int32
	r.fetch = markdownFetch("loading", "App", &calls)
	r.render = func(context.Context, string) (FetchResult, bool, error) {
		return FetchResult{}, false, errors.New("no render lane available")
	}

	first, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1 (second Run must hit the fresh cache)", calls.Load())
	}
	for _, out := range []string{first, second} {
		if !strings.Contains(out, "set JINA_API_KEY") {
			t.Errorf("outline missing the thin note:\n%s", out)
		}
	}
}

func TestRunNotThinNeverCallsRenderPage(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	r := testRunner()
	r.fetch = markdownFetch(fixtureMarkdown, "Guide", nil)
	r.render = func(context.Context, string) (FetchResult, bool, error) {
		t.Error("renderPage called for a non-thin page")
		return FetchResult{}, false, nil
	}

	out, err := r.run(ctx, backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "static content") || strings.Contains(out, "set JINA_API_KEY") {
		t.Errorf("a non-thin page carried a thin note:\n%s", out)
	}
}

func TestRunThinReEscalatesOn304(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t, "PATH=")
	r := testRunner()

	// A stale Thin page whose origin will 304: without re-escalation it would trap
	// Thin forever, breaking the note's own "re-run with --refresh" promise.
	prior := samplePage(fixtureURL, 1, 0, EmbedModelID)
	prior.Thin = true
	prior.FetchedAt = fixtureNow.Add(-48 * time.Hour)
	if err := r.store.save(ctx, prior); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	r.fetch = func(_ context.Context, _ string, p *Page) (FetchResult, error) {
		if p == nil {
			t.Error("revalidation fetch got a nil prior")
		}
		return FetchResult{}, fmt.Errorf("http: %w", ErrNotModified)
	}
	rendered := "# Rendered\n\n" + strings.Repeat("real rendered prose here. ", 20)
	r.render = func(context.Context, string) (FetchResult, bool, error) {
		return FetchResult{Tier: TierJinaRender, FinalURL: fixtureURL, Title: "Rendered", Markdown: rendered}, false, nil
	}

	out, err := r.run(ctx, backend.OpWebRead, backend.Args{URL: fixtureURL, Full: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(out, rendered) {
		t.Errorf("304 re-escalation did not serve the rendered body:\n%s", out)
	}
	if strings.Contains(out, "static content") {
		t.Errorf("re-escalated page still carries a thin note:\n%s", out)
	}
	page, _ := r.store.load(ctx, fixtureURL, EmbedModelID)
	if page == nil || page.Thin {
		t.Errorf("page.Thin = %v, want false after 304 re-escalation", page != nil && page.Thin)
	}
	if page != nil && page.Markdown != rendered {
		t.Errorf("persisted markdown is not the rendered body:\n%s", page.Markdown)
	}
}

func TestThinNoteLocalTargetNamesAgentBrowser(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t, envJinaKey+"=jina-key", "PATH=")

	note := thinNote(ctx, &Page{URL: "http://localhost:3000/app", Thin: true})
	if !strings.Contains(note, "install agent-browser") {
		t.Errorf("local thin note = %q, want it to name agent-browser install", note)
	}
	if strings.Contains(note, "may genuinely have little static content") {
		t.Errorf("local thin note wrongly claims genuine-little-content:\n%s", note)
	}
}
