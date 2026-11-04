package dispatch

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/workspace"
)

// slowFakeEmbedder maps each text to a deterministic unit vector, standing in for
// the resident WASM engine so runSemantic runs without weights.
type slowFakeEmbedder struct{}

func (slowFakeEmbedder) Dims() int { return 16 }

func (slowFakeEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = dispatchUnitVec(text, 16)
	}
	return out, nil
}

func dispatchUnitVec(s string, dims int) []float32 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, s)
	r := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // deterministic test vectors
	v := make([]float32, dims)
	var norm float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func writeDispatchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "package auth\n\nfunc Login(user, session string) error {\n\treturn nil\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "auth.go"), []byte(body), 0o600); err != nil {
		t.Fatalf("write repo: %v", err)
	}
	return dir
}

// TestRunSemanticSlowNoteCountsColdInit covers the SEAM fix: the latency timer
// must start before the embedder is constructed, so a slow cold first request
// (weight download, WASM compile, model load — here a delayed provider) trips the
// slow-search note. Before the fix the timer started after sharedEmbedder, so the
// cold-init latency was excluded and the note was wrongly omitted.
func TestRunSemanticSlowNoteCountsColdInit(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	engine.CloseIndexCache()
	t.Cleanup(engine.CloseIndexCache)

	prevThreshold := render.SlowSearchThreshold
	render.SlowSearchThreshold = time.Second
	t.Cleanup(func() { render.SlowSearchThreshold = prevThreshold })

	const coldInit = 1500 * time.Millisecond
	restore := SetEmbedderProvider(func(_ context.Context) (index.Embedder, error) {
		time.Sleep(coldInit) // the cold-init cost the timer must include
		return slowFakeEmbedder{}, nil
	})
	t.Cleanup(restore)

	repo := writeDispatchRepo(t)
	out, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: repo, K: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "slow search") {
		t.Errorf("slow-search note missing; cold-init latency must count toward the timer:\n%s", out)
	}
}

// TestRunGrepSlowNote covers the OpGrep arm disclosing an expensive walk: a
// dependency-tree grep costs tens of seconds today and returns with no sign it
// was expensive.
func TestRunGrepSlowNote(t *testing.T) {
	repo := writeDispatchRepo(t)

	tests := []struct {
		name      string
		threshold time.Duration
		paths     []string
		want      string
		wantNote  bool
	}{
		{
			name:      "fast walk stays silent",
			threshold: time.Hour,
			paths:     []string{repo},
		},
		{
			name:      "slow walk names the path operand",
			threshold: 0,
			paths:     []string{repo},
			want:      "walked " + repo + "; narrow with a subpath or --glob\n",
			wantNote:  true,
		},
		{
			name:      "slow walk with no operand names the working directory",
			threshold: 0,
			want:      "walked the working directory; narrow with a subpath or --glob\n",
			wantNote:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(repo)
			prev := render.SlowGrepThreshold
			render.SlowGrepThreshold = tt.threshold
			t.Cleanup(func() { render.SlowGrepThreshold = prev })

			out, err := Run(context.Background(), backend.OpGrep, backend.Args{Query: "Login", Paths: tt.paths, Budget: ripgrep.DefaultBudget})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(out, "auth.go") {
				t.Fatalf("grep results missing the hit:\n%s", out)
			}
			if got := strings.Contains(out, "# note: slow grep"); got != tt.wantNote {
				t.Fatalf("slow-grep note = %v, want %v:\n%s", got, tt.wantNote, out)
			}
			if tt.wantNote && !strings.HasSuffix(out, tt.want) {
				t.Errorf("output does not end with %q:\n%s", tt.want, out)
			}
		})
	}
}

type releasingEmbedder struct {
	slowFakeEmbedder
	lastUsed time.Time
	cutoffs  []time.Time
}

func (e *releasingEmbedder) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	e.lastUsed = time.Now()
	return e.slowFakeEmbedder.Encode(ctx, texts)
}

func (e *releasingEmbedder) ReleaseIdle(_ context.Context, cutoff time.Time) (bool, error) {
	e.cutoffs = append(e.cutoffs, cutoff)
	return e.lastUsed.Before(cutoff), nil
}

func TestSweepIdleFreesOnlyPastTheCutoff(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	engine.CloseIndexCache()
	t.Cleanup(engine.CloseIndexCache)

	emb := &releasingEmbedder{}
	restore := SetEmbedderProvider(func(context.Context) (index.Embedder, error) { return emb, nil })
	t.Cleanup(restore)

	repo := writeDispatchRepo(t)
	if _, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: repo, K: 3}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	before := time.Now().Add(-time.Hour)
	freed, err := SweepIdle(context.Background(), before)
	if err != nil || freed {
		t.Fatalf("SweepIdle(an hour ago) = %v, %v; want false, nil right after a search", freed, err)
	}

	after := time.Now().Add(time.Second)
	freed, err = SweepIdle(context.Background(), after)
	if err != nil || !freed {
		t.Fatalf("SweepIdle(after the search) = %v, %v; want true, nil", freed, err)
	}
	if engine.SweepIdle(after.Add(time.Hour)) != 0 {
		t.Error("SweepIdle left a resident index behind")
	}
	if len(emb.cutoffs) != 2 || !emb.cutoffs[0].Equal(before) || !emb.cutoffs[1].Equal(after) {
		t.Errorf("embedder saw cutoffs %v, want [%v %v]", emb.cutoffs, before, after)
	}
}

func TestRunSemanticRefusesAnythingButACheckout(t *testing.T) {
	realHome := func(t *testing.T) string {
		t.Helper()
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		return home
	}
	mkdir := func(t *testing.T, parts ...string) string {
		t.Helper()
		dir := filepath.Join(parts...)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name    string
		root    func(t *testing.T, home string) string
		refused bool
	}{
		{"the home directory", func(_ *testing.T, home string) string { return home }, true},
		{"a directory holding the home directory", func(_ *testing.T, home string) string { return filepath.Dir(home) }, true},
		{"a folder of repositories under home", func(t *testing.T, home string) string { return mkdir(t, home, "Code") }, true},
		{"a directory outside any repository", func(t *testing.T, _ string) string { return t.TempDir() }, true},
		{"the home directory when it is a dotfiles repository", func(t *testing.T, home string) string {
			mkdir(t, home, ".git")
			return home
		}, true},
		{"a folder under a dotfiles home", func(t *testing.T, home string) string {
			mkdir(t, home, ".git")
			return mkdir(t, home, "Code")
		}, true},
		{"a git checkout under home", func(t *testing.T, home string) string {
			repo := mkdir(t, home, "Code", "repo")
			mkdir(t, repo, ".git")
			return repo
		}, false},
		{"a directory inside a git checkout", func(t *testing.T, home string) string {
			repo := mkdir(t, home, "Code", "repo")
			mkdir(t, repo, ".git")
			return mkdir(t, repo, "pkg")
		}, false},
		{"a jj checkout", func(t *testing.T, home string) string {
			repo := mkdir(t, home, "Code", "repo")
			mkdir(t, repo, ".jj")
			return repo
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := realHome(t)
			err := requireCheckout(tt.root(t, home))
			if got := errors.Is(err, errNotCheckout); got != tt.refused {
				t.Errorf("requireCheckout() = %v, want refused %t", err, tt.refused)
			}
		})
	}
}

func TestRunSemanticRefusalNeverLoadsTheEmbedder(t *testing.T) {
	root := t.TempDir()
	restore := SetEmbedderProvider(func(context.Context) (index.Embedder, error) {
		t.Fatal("a refused root constructed the embedder")
		return nil, nil
	})
	t.Cleanup(restore)
	for _, op := range []backend.Op{backend.OpSearch, backend.OpRelated} {
		if _, err := Run(context.Background(), op, backend.Args{Query: "login", Path: root}); !errors.Is(err, errNotCheckout) {
			t.Errorf("Run(%s) = %v, want %v", op, err, errNotCheckout)
		}
	}
}

type countingEmbedder struct {
	slowFakeEmbedder
	texts *atomic.Int64
}

func (e countingEmbedder) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	e.texts.Add(int64(len(texts)))
	return e.slowFakeEmbedder.Encode(ctx, texts)
}

// confinedServer stands in for a ccx mcp server started in start: builds are
// confined, the client declared start, and the index cache is empty and private.
// It returns the cache's semsearch directory.
func confinedServer(t *testing.T, start string) string {
	t.Helper()
	data := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", data)
	engine.CloseIndexCache()
	t.Cleanup(engine.CloseIndexCache)
	workspace.SetRoot(start)
	t.Cleanup(func() { workspace.SetRoot("") })
	t.Cleanup(ConfineIndexBuilds())
	return filepath.Join(data, "semsearch")
}

func fakeHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

func TestConfinedServerRefusesToBuildAnotherRoot(t *testing.T) {
	tests := []struct {
		name  string
		start func(t *testing.T, home string) string
		ctx   func(ctx context.Context, repo string) context.Context
		path  func(repo string) string
	}{
		{name: "a home start naming a repo", start: func(_ *testing.T, home string) string { return home }},
		{name: "a dotfiles home start naming a repo", start: func(t *testing.T, home string) string {
			if err := os.Mkdir(filepath.Join(home, ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			return home
		}},
		{name: "a start outside any checkout naming a repo", start: func(t *testing.T, _ string) string { return t.TempDir() }},
		{name: "repo A naming repo B", start: func(t *testing.T, _ string) string { return writeDispatchRepo(t) }},
		{
			name:  "repo A with repo B pinned as the call's root",
			start: func(t *testing.T, _ string) string { return writeDispatchRepo(t) },
			ctx:   workspace.WithRoot,
			path:  func(string) string { return "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheDir := confinedServer(t, tt.start(t, fakeHome(t)))
			repo := writeDispatchRepo(t)
			restore := SetEmbedderProvider(func(context.Context) (index.Embedder, error) {
				t.Fatal("a refused root constructed the embedder")
				return nil, nil
			})
			t.Cleanup(restore)
			ctx := context.Background()
			path := repo
			if tt.ctx != nil {
				ctx = tt.ctx(ctx, repo)
			}
			if tt.path != nil {
				path = tt.path(repo)
			}

			for op, query := range map[backend.Op]string{backend.OpSearch: "login", backend.OpRelated: "auth.go:3"} {
				_, err := Run(ctx, op, backend.Args{Query: query, Path: path})
				if !errors.Is(err, errNotStartedCheckout) {
					t.Fatalf("Run(%s) = %v, want %v", op, err, errNotStartedCheckout)
				}
				if !strings.Contains(err.Error(), "ccx code grep") {
					t.Errorf("Run(%s) error = %q, want it to point at ccx code grep", op, err)
				}
			}
			entries, err := os.ReadDir(cacheDir)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("index cache holds %d entries after a refusal, want none", len(entries))
			}
		})
	}
}

func TestConfinedServerBuildsItsOwnCheckout(t *testing.T) {
	repo := writeDispatchRepo(t)
	sub := filepath.Join(repo, "pkg")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "login.go"), []byte("package pkg\n\nfunc Login() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"with no repo named", ""},
		{"naming itself", repo},
		{"naming a directory inside itself", sub},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			confinedServer(t, repo)
			restore := SetEmbedderProvider(func(context.Context) (index.Embedder, error) { return slowFakeEmbedder{}, nil })
			t.Cleanup(restore)

			out, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: tt.path, K: 3})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(out, "login.go") && !strings.Contains(out, "auth.go") {
				t.Errorf("output = %q, want a hit from the built index", out)
			}
			if strings.Contains(out, "existing index") {
				t.Errorf("output = %q, want no unrefreshed-index note on the server's own checkout", out)
			}
		})
	}
}

func TestConfinedServerAnswersFromAnExistingIndex(t *testing.T) {
	tests := []struct {
		name  string
		start func(t *testing.T, home string) string
	}{
		{"from a home start", func(_ *testing.T, home string) string { return home }},
		{"from another repo", func(t *testing.T, _ string) string { return writeDispatchRepo(t) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := fakeHome(t)
			repo := writeDispatchRepo(t)
			start := tt.start(t, home)
			t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
			engine.CloseIndexCache()
			t.Cleanup(engine.CloseIndexCache)
			texts := &atomic.Int64{}
			restore := SetEmbedderProvider(func(context.Context) (index.Embedder, error) {
				return countingEmbedder{texts: texts}, nil
			})
			t.Cleanup(restore)
			if _, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: repo, K: 3}); err != nil {
				t.Fatalf("build the index unconfined: %v", err)
			}
			engine.CloseIndexCache()
			if err := os.WriteFile(filepath.Join(repo, "added.go"), []byte("package auth\n\nfunc Added() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			workspace.SetRoot(start)
			t.Cleanup(func() { workspace.SetRoot("") })
			t.Cleanup(ConfineIndexBuilds())
			texts.Store(0)

			out, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: repo, K: 3})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(out, "auth.go") {
				t.Errorf("output = %q, want a hit from the existing index", out)
			}
			if strings.Contains(out, "added.go") {
				t.Errorf("output = %q, want the existing index unrefreshed", out)
			}
			if !strings.Contains(out, "existing index") {
				t.Errorf("output = %q, want the unrefreshed-index note", out)
			}
			if got := texts.Load(); got != 1 {
				t.Errorf("embedded %d texts, want only the query", got)
			}
		})
	}
}

func TestRunSemanticCapsTheProcessCPU(t *testing.T) {
	prev := runtime.GOMAXPROCS(maxSemanticProcs * 2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	engine.CloseIndexCache()
	t.Cleanup(engine.CloseIndexCache)
	restore := SetEmbedderProvider(func(context.Context) (index.Embedder, error) { return slowFakeEmbedder{}, nil })
	t.Cleanup(restore)

	if _, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: writeDispatchRepo(t), K: 3}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runtime.GOMAXPROCS(0); got != maxSemanticProcs {
		t.Errorf("GOMAXPROCS after a semantic search = %d, want %d", got, maxSemanticProcs)
	}
}
