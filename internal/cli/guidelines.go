package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	guidelinesSchema = 1
	guidelinesTTL    = 24 * time.Hour
	guidelinesBudget = 4000
	guidelinesFile   = "guidelines.json"

	// guidelinesCharsPerToken mirrors render.Cap's byte-to-token ratio so a
	// document's --json truncation fields describe the same cut render.Cap's
	// human-mode footer announces.
	guidelinesCharsPerToken = 4
)

const (
	guidelinesKindPRTemplate    = "pr-template"
	guidelinesKindContributing  = "contributing"
	guidelinesKindCodeOfConduct = "code-of-conduct"
	guidelinesKindIssueConfig   = "issue-config"
	guidelinesKindContactLinks  = "contact-links"
	guidelinesKindIssueTemplate = "issue-template"

	guidelinesSourceLocal  = "local"
	guidelinesSourceGitHub = "github"
)

const (
	guidelinesRepoFields       = "nameWithOwner,pullRequestTemplates,codeOfConduct,contactLinks,issueTemplates"
	guidelinesRawAccept        = "Accept: application/vnd.github.raw"
	guidelinesIssueTemplateDir = ".github/ISSUE_TEMPLATE"
	guidelinesIssueConfigPath  = guidelinesIssueTemplateDir + "/config.yml"
)

// guidelinesKinds is both the emit order and the completeness checklist: a kind
// with no document lands in Missing. The PR template leads because that is the
// document a caller drafting a pull request has to reproduce.
var guidelinesKinds = []string{
	guidelinesKindPRTemplate,
	guidelinesKindContributing,
	guidelinesKindCodeOfConduct,
	guidelinesKindIssueConfig,
	guidelinesKindContactLinks,
	guidelinesKindIssueTemplate,
}

var (
	// guidelinesContributingPaths are the CONTRIBUTING locations GitHub itself
	// recognizes, in resolution order.
	guidelinesContributingPaths = func() []string {
		dirs, exts := []string{"", ".github/", "docs/"}, []string{".md", ".rst", ".txt", ""}
		paths := make([]string, 0, len(dirs)*len(exts))
		for _, dir := range dirs {
			for _, ext := range exts {
				paths = append(paths, dir+"CONTRIBUTING"+ext)
			}
		}
		return paths
	}()

	guidelinesIssueConfigPaths = []string{guidelinesIssueConfigPath, guidelinesIssueTemplateDir + "/config.yaml"}

	guidelinesTemplateDirs = []string{
		"", ".github/", "docs/",
		".github/PULL_REQUEST_TEMPLATE/", "PULL_REQUEST_TEMPLATE/", "docs/PULL_REQUEST_TEMPLATE/",
	}

	// guidelinesCLA matches the acronym on a word boundary, so "clarify" does not
	// fire the signal. A mention the literal match cannot settle leaves the signal
	// false — the documents themselves are served for the caller to read.
	guidelinesCLA = regexp.MustCompile(`\bCLA\b`)

	guidelinesSignalLabels = []struct{ key, label string }{
		{"signoff_required", "signoff"},
		{"cla", "cla"},
		{"conventional_commits", "conventional-commits"},
	}
)

// guidelinesDoc is one contribution document, served verbatim.
type guidelinesDoc struct {
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	Source        string `json:"source"`
	Tokens        int    `json:"tokens"`
	Truncated     bool   `json:"truncated,omitempty"`
	OmittedTokens int    `json:"omitted_tokens,omitempty"`
	Body          string `json:"body"`

	// raw is the pre-budget body, which the human writer re-caps through
	// render.Cap to get its omission footer.
	raw string
}

// guidelines is the answer for one repo: the documents, the rules they state
// literally, and the kinds that are absent.
type guidelines struct {
	Repo      string          `json:"repo"`
	FetchedAt time.Time       `json:"fetched_at"`
	Cached    bool            `json:"cached"`
	CachePath string          `json:"cache_path"`
	Documents []guidelinesDoc `json:"documents"`
	Signals   map[string]bool `json:"signals"`
	Missing   []string        `json:"missing"`
}

type guidelinesPRTemplate struct {
	Filename string `json:"filename"`
	Body     string `json:"body"`
}

type guidelinesCodeOfConduct struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type guidelinesContactLink struct {
	About string `json:"about"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

type guidelinesIssueTemplate struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	About string `json:"about"`
	Body  string `json:"body"`
}

// guidelinesRepoView is the one gh repo view payload: it resolves the root,
// docs/, .github/, and the multi-template .github/PULL_REQUEST_TEMPLATE/
// directory server-side, so every GitHub-side kind costs a single call.
type guidelinesRepoView struct {
	NameWithOwner        string                    `json:"nameWithOwner"`
	PullRequestTemplates []guidelinesPRTemplate    `json:"pullRequestTemplates"`
	CodeOfConduct        *guidelinesCodeOfConduct  `json:"codeOfConduct"`
	ContactLinks         []guidelinesContactLink   `json:"contactLinks"`
	IssueTemplates       []guidelinesIssueTemplate `json:"issueTemplates"`
}

type guidelinesContributingFile struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

// guidelinesEnvelope is the cached GitHub payload. Local documents are absent by
// design: a branch switch rewrites CONTRIBUTING.md on disk while the GitHub
// payload stays valid, and reading a local file costs nothing.
type guidelinesEnvelope struct {
	Schema       int                         `json:"schema"`
	FetchedAt    time.Time                   `json:"fetched_at"`
	Repo         guidelinesRepoView          `json:"repo"`
	Contributing *guidelinesContributingFile `json:"contributing,omitempty"`
	Probed       bool                        `json:"contributing_probed"`
}

// valid reports whether env still answers for now: a matching schema and a fetch
// inside the TTL. A schema mismatch or a zero fetch time reads as a miss.
func (env *guidelinesEnvelope) valid(now time.Time) bool {
	return env.Schema == guidelinesSchema && now.Sub(env.FetchedAt) < guidelinesTTL
}

// applyBudget trims the document to budget tokens the way render.Cap does and
// records the cut, since --json carries no omission footer.
func (d *guidelinesDoc) applyBudget(budget int) {
	kept, omitted, trimmed := guidelinesTrim(d.raw, budget)
	d.Body = kept
	d.Tokens = len(kept) / guidelinesCharsPerToken
	d.Truncated = trimmed
	d.OmittedTokens = len(omitted) / guidelinesCharsPerToken
}

type guidelinesOpts struct {
	emitJSON bool
	refresh  bool
	full     bool
	budget   int
}

// newGuidelinesCmd builds `ccx vcs guidelines`, which caches raw documents rather
// than summaries: ccx has no generative model, the caller must reproduce a PR
// template verbatim down to its checkboxes and <!-- --> guidance, a cached
// document is falsifiable where a cached summary is not, and render.Cap already
// degrades an oversized document honestly with an explicit omission footer.
func newGuidelinesCmd() *cobra.Command {
	var o guidelinesOpts
	cmd := &cobra.Command{
		Use:     "guidelines",
		Aliases: []string{"contributing"},
		Short:   "Fetch and cache a repo's stated contribution rules",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGuidelines(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.emitJSON, "json", false, "emit the documents as JSON")
	cmd.Flags().BoolVar(&o.refresh, "refresh", false, "re-fetch the GitHub payload instead of serving the cache")
	cmd.Flags().BoolVar(&o.full, "full", false, "include issue-template bodies")
	cmd.Flags().IntVar(&o.budget, "budget", guidelinesBudget, "token budget per document (0 = uncapped)")
	return cmd
}

func runGuidelines(cmd *cobra.Command, o guidelinesOpts) error {
	root, err := guidelinesRoot()
	if err != nil {
		return err
	}
	dir, err := guidelinesCacheDir(root)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, guidelinesFile)

	local := localGuidelinesDocs(root)
	_, haveContributing := local[guidelinesKindContributing]

	env, cached := loadGuidelines(path, time.Now(), o.refresh, !haveContributing)
	if !cached {
		env, err = refreshGuidelines(cmd, render.Dir(root), dir, path, !haveContributing)
		if err != nil {
			return err
		}
	}

	docs := guidelinesDocuments(root, env, local, o.full)
	g := guidelines{
		Repo:      guidelinesRepoName(env, root),
		FetchedAt: env.FetchedAt,
		Cached:    cached,
		CachePath: path,
		Documents: docs,
		Signals:   guidelinesSignals(docs),
		Missing:   guidelinesMissing(docs),
	}
	for i := range g.Documents {
		g.Documents[i].applyBudget(o.budget)
	}

	if o.emitJSON {
		return writeGuidelinesJSON(cmd.OutOrStdout(), g)
	}
	return writeGuidelinesHuman(cmd.OutOrStdout(), g, o.budget)
}

// refreshGuidelines fetches and caches the GitHub payload. A gh that cannot
// answer degrades to the local documents with a note on stderr instead of
// failing the command, and the failure is never cached.
func refreshGuidelines(cmd *cobra.Command, root render.Dir, dir, path string, needContributing bool) (guidelinesEnvelope, error) {
	ctx := cmd.Context()
	env, err := fetchGuidelines(ctx, root, needContributing)
	if err != nil {
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "guidelines: %v — serving local documents only\n", err); werr != nil {
			return guidelinesEnvelope{}, werr
		}
		return guidelinesEnvelope{}, nil
	}
	if err := storeGuidelines(ctx, dir, path, env); err != nil {
		return guidelinesEnvelope{}, err
	}
	return env, nil
}

// guidelinesRoot resolves the repo root as an absolute, symlink-free path, so a
// relative path and a symlinked one key the same cache entry.
func guidelinesRoot() (string, error) {
	c, err := vcs.ResolveCheckout(workingDir())
	if err != nil {
		return "", fmt.Errorf("guidelines: %w", err)
	}
	return c.Root, nil
}

// guidelinesCacheDir resolves the per-repo cache directory out of the GitHub
// metadata record's own path, so the documents — pure repository identity —
// cannot drift into a per-checkout entry.
func guidelinesCacheDir(root string) (string, error) {
	repoPath, err := vcs.RepoCachePath(root)
	if err != nil {
		return "", err
	}
	return filepath.Dir(repoPath), nil
}

// loadGuidelines serves the cached GitHub payload when it is fresh and answers
// the question at hand: an envelope written while CONTRIBUTING was on disk never
// probed the community-profile fallback, so a run that now needs it is a miss.
func loadGuidelines(path string, now time.Time, refresh, needContributing bool) (guidelinesEnvelope, bool) {
	if refresh {
		return guidelinesEnvelope{}, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is ccx's own per-repo cache entry, not untrusted input
	if err != nil {
		return guidelinesEnvelope{}, false
	}
	var env guidelinesEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return guidelinesEnvelope{}, false
	}
	if !env.valid(now) || (needContributing && !env.Probed) {
		return guidelinesEnvelope{}, false
	}
	return env, true
}

func storeGuidelines(ctx context.Context, dir, path string, env guidelinesEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("guidelines: encode cache: %w", err)
	}
	return cache.WithLock(ctx, dir, "guidelines", func() error {
		return cache.Store(path, data, 0o600)
	})
}

// fetchGuidelines batches every GitHub-side field into one gh repo view, adding
// the community-profile lookup only when the local CONTRIBUTING candidates all
// missed.
func fetchGuidelines(ctx context.Context, root render.Dir, needContributing bool) (guidelinesEnvelope, error) {
	out, err := render.RunCLI(ctx, root, "gh", []string{"repo", "view", "--json", guidelinesRepoFields})
	if err != nil {
		return guidelinesEnvelope{}, fmt.Errorf("gh repo view: %w", err)
	}
	var view guidelinesRepoView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return guidelinesEnvelope{}, fmt.Errorf("parse gh repo view: %w", err)
	}
	env := guidelinesEnvelope{Schema: guidelinesSchema, FetchedAt: time.Now(), Repo: view}
	if !needContributing {
		return env, nil
	}
	file, err := fetchGuidelinesContributing(ctx, view.NameWithOwner)
	if err != nil {
		return guidelinesEnvelope{}, err
	}
	env.Contributing, env.Probed = file, true
	return env, nil
}

// fetchGuidelinesContributing resolves CONTRIBUTING server-side: GraphQL has no
// contributingGuidelines field, but the community profile names the contents URL
// of whichever candidate GitHub found.
func fetchGuidelinesContributing(ctx context.Context, nwo string) (*guidelinesContributingFile, error) {
	out, err := render.RunCLI(ctx, render.Ambient, "gh", []string{"api", "repos/" + nwo + "/community/profile"})
	if err != nil {
		return nil, fmt.Errorf("gh api community profile: %w", err)
	}
	var profile struct {
		Files struct {
			Contributing *struct {
				URL string `json:"url"`
			} `json:"contributing"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &profile); err != nil {
		return nil, fmt.Errorf("parse community profile: %w", err)
	}
	if profile.Files.Contributing == nil {
		return nil, nil
	}
	url := profile.Files.Contributing.URL
	body, err := render.RunCLI(ctx, render.Ambient, "gh", []string{"api", url, "-H", guidelinesRawAccept})
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", url, err)
	}
	return &guidelinesContributingFile{Path: guidelinesContentsPath(url), Body: body}, nil
}

// guidelinesContentsPath reads the repo-relative path out of a contents API URL
// (.../repos/OWNER/REPO/contents/.github/CONTRIBUTING.md).
func guidelinesContentsPath(url string) string {
	_, rel, ok := strings.Cut(url, "/contents/")
	if !ok {
		return url
	}
	path, _, _ := strings.Cut(rel, "?")
	return path
}

// localGuidelinesDocs reads the on-disk documents fresh on every run: a branch
// switch rewrites them while the cached GitHub payload stays valid.
func localGuidelinesDocs(root string) map[string]guidelinesDoc {
	docs := make(map[string]guidelinesDoc, 2)
	for _, rel := range guidelinesContributingPaths {
		if doc, ok := readGuidelinesFile(root, rel, guidelinesKindContributing); ok {
			docs[guidelinesKindContributing] = doc
			break
		}
	}
	for _, rel := range guidelinesIssueConfigPaths {
		if doc, ok := readGuidelinesFile(root, rel, guidelinesKindIssueConfig); ok {
			docs[guidelinesKindIssueConfig] = doc
			break
		}
	}
	return docs
}

func readGuidelinesFile(root, rel, kind string) (guidelinesDoc, bool) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // rel is one of ccx's fixed candidate paths, not untrusted input
	if err != nil {
		return guidelinesDoc{}, false
	}
	return guidelinesDoc{Kind: kind, Path: rel, Source: guidelinesSourceLocal, raw: string(data)}, true
}

// guidelinesDocuments merges the fresh local reads with the GitHub payload in
// guidelinesKinds order, preferring the local CONTRIBUTING over the fetched one
// because it matches the checked-out revision.
func guidelinesDocuments(root string, env guidelinesEnvelope, local map[string]guidelinesDoc, full bool) []guidelinesDoc {
	docs := make([]guidelinesDoc, 0, len(guidelinesKinds))

	templates := slices.Clone(env.Repo.PullRequestTemplates)
	slices.SortFunc(templates, func(a, b guidelinesPRTemplate) int { return strings.Compare(a.Filename, b.Filename) })
	for _, template := range templates {
		docs = append(docs, guidelinesDoc{
			Kind:   guidelinesKindPRTemplate,
			Path:   guidelinesTemplatePath(root, template.Filename),
			Source: guidelinesSourceGitHub,
			raw:    template.Body,
		})
	}

	if doc, ok := local[guidelinesKindContributing]; ok {
		docs = append(docs, doc)
	} else if env.Contributing != nil {
		docs = append(docs, guidelinesDoc{
			Kind:   guidelinesKindContributing,
			Path:   env.Contributing.Path,
			Source: guidelinesSourceGitHub,
			raw:    env.Contributing.Body,
		})
	}

	if coc := env.Repo.CodeOfConduct; coc != nil {
		docs = append(docs, guidelinesDoc{
			Kind:   guidelinesKindCodeOfConduct,
			Path:   coc.URL,
			Source: guidelinesSourceGitHub,
			raw:    coc.Name + "\n",
		})
	}

	if doc, ok := local[guidelinesKindIssueConfig]; ok {
		docs = append(docs, doc)
	}

	if body := guidelinesContactBody(env.Repo.ContactLinks); body != "" {
		docs = append(docs, guidelinesDoc{
			Kind:   guidelinesKindContactLinks,
			Path:   guidelinesIssueConfigPath,
			Source: guidelinesSourceGitHub,
			raw:    body,
		})
	}

	if body := guidelinesIssueBody(env.Repo.IssueTemplates, full); body != "" {
		docs = append(docs, guidelinesDoc{
			Kind:   guidelinesKindIssueTemplate,
			Path:   guidelinesIssueTemplateDir,
			Source: guidelinesSourceGitHub,
			raw:    body,
		})
	}

	return docs
}

// guidelinesTemplatePath locates the PR template gh named: the payload carries a
// bare filename, and every candidate directory is a fixed GitHub location.
func guidelinesTemplatePath(root, filename string) string {
	for _, dir := range guidelinesTemplateDirs {
		rel := dir + filename
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			return rel
		}
	}
	return filename
}

func guidelinesContactBody(links []guidelinesContactLink) string {
	var b strings.Builder
	for _, link := range links {
		fmt.Fprintf(&b, "%s (%s)\n", guidelinesSummary(link.Name, link.About), link.URL)
	}
	return b.String()
}

// guidelinesIssueBody lists the issue templates by name; --full adds the bodies,
// which run to multiple KB each and say nothing about opening a pull request.
func guidelinesIssueBody(templates []guidelinesIssueTemplate, full bool) string {
	var b strings.Builder
	for _, template := range templates {
		b.WriteString(guidelinesSummary(template.Name, template.About))
		b.WriteByte('\n')
		if full && template.Body != "" {
			b.WriteByte('\n')
			b.WriteString(strings.TrimRight(template.Body, "\n"))
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// guidelinesSummary renders "<name> — <about>", dropping the dash when about is
// empty.
func guidelinesSummary(name, about string) string {
	if about == "" {
		return name
	}
	return name + " — " + about
}

func guidelinesRepoName(env guidelinesEnvelope, root string) string {
	if env.Repo.NameWithOwner != "" {
		return env.Repo.NameWithOwner
	}
	return filepath.Base(root)
}

// guidelinesSignals reports the contribution rules the documents state literally.
// It never infers or ranks: a rule the literal match cannot settle stays false.
func guidelinesSignals(docs []guidelinesDoc) map[string]bool {
	var b strings.Builder
	for _, doc := range docs {
		b.WriteString(doc.raw)
		b.WriteByte('\n')
	}
	text := b.String()
	lower := strings.ToLower(text)
	return map[string]bool{
		"signoff_required":     strings.Contains(lower, "signed-off-by"),
		"cla":                  guidelinesCLA.MatchString(text) || strings.Contains(lower, "contributor license agreement"),
		"conventional_commits": strings.Contains(lower, "conventional commits"),
	}
}

func guidelinesMissing(docs []guidelinesDoc) []string {
	present := make(map[string]bool, len(docs))
	for _, doc := range docs {
		present[doc.Kind] = true
	}
	missing := make([]string, 0, len(guidelinesKinds))
	for _, kind := range guidelinesKinds {
		if !present[kind] {
			missing = append(missing, kind)
		}
	}
	return missing
}

// guidelinesTrim splits body at the last line boundary within budget tokens,
// mirroring render.Cap's cut so a document's --json fields and its human-mode
// footer report the same omission.
func guidelinesTrim(body string, budget int) (kept, omitted string, trimmed bool) {
	if budget <= 0 {
		return body, "", false
	}
	limit := budget * guidelinesCharsPerToken
	if limit/guidelinesCharsPerToken != budget || len(body) <= limit {
		return body, "", false
	}
	cut := strings.LastIndexByte(body[:limit], '\n')
	if cut < 0 {
		cut = guidelinesRuneStart(body, limit)
	}
	return body[:cut], body[cut:], true
}

// guidelinesRuneStart moves i back to the first byte of the rune it lands in, so
// a body with no line boundary in budget is still cut where render.Cap cuts it.
func guidelinesRuneStart(s string, i int) int {
	for j := i; j >= 0 && i-j < utf8.UTFMax; j-- {
		if utf8.RuneStart(s[j]) {
			return j
		}
	}
	return i
}

func writeGuidelinesJSON(w io.Writer, g guidelines) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("guidelines: encode json: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return err
	}
	return nil
}

// writeGuidelinesHuman renders the documents under "## " headers so a consumer
// can split the output on them.
func writeGuidelinesHuman(w io.Writer, g guidelines, budget int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# guidelines %s · %d documents · %s\n", g.Repo, len(g.Documents), guidelinesFreshness(g, time.Now()))
	for _, doc := range g.Documents {
		fmt.Fprintf(&b, "\n## %s %s (%s · %d tokens)\n", doc.Kind, doc.Path, doc.Source, doc.Tokens)
		if body := strings.TrimRight(render.Cap(doc.raw, budget), "\n"); body != "" {
			b.WriteString(body)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	if len(g.Missing) > 0 {
		fmt.Fprintf(&b, "# missing: %s\n", strings.Join(g.Missing, ", "))
	}
	fmt.Fprintf(&b, "# signals: %s\n", guidelinesSignalLine(g.Signals))
	fmt.Fprintf(&b, "# cache: %s\n", g.CachePath)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return nil
}

func guidelinesFreshness(g guidelines, now time.Time) string {
	switch {
	case g.FetchedAt.IsZero():
		return "no github payload"
	case g.Cached:
		return fmt.Sprintf("cached %s ago (--refresh to re-fetch)", guidelinesAge(now.Sub(g.FetchedAt)))
	default:
		return "fetched now"
	}
}

// guidelinesAge renders d at one unit of resolution ("35s", "4m", "2h").
func guidelinesAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func guidelinesSignalLine(signals map[string]bool) string {
	parts := make([]string, 0, len(guidelinesSignalLabels))
	for _, signal := range guidelinesSignalLabels {
		state := "no"
		if signals[signal.key] {
			state = "yes"
		}
		parts = append(parts, signal.label+"="+state)
	}
	return strings.Join(parts, " ")
}
