package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/cache"
)

// ToolInfo describes one reflected MCP tool as a sandbox host function.
// Destructive mirrors the annotation's nil-able pointer — nil means the server
// didn't say, which is not the same as false; it only ever labels the
// signature, never filters the tool.
type ToolInfo struct {
	Server      string
	Tool        string
	FuncName    string
	Params      []string
	Summary     string
	ReadOnly    bool
	Destructive *bool
}

// ServerCatalog is one server's reflected tools; Note carries the classifier
// warning or the failure that left it empty.
type ServerCatalog struct {
	Spec  ServerSpec
	Tools []ToolInfo
	Note  string
}

// Catalog is the reflected tool surface built for one inventory hash.
type Catalog struct {
	Hash    string
	Built   time.Time
	Servers []ServerCatalog
}

// CatalogStore caches catalogs across engine instances. WithLock serializes
// concurrent rebuilds; a loser re-Loads the winner's catalog inside its
// critical section instead of building again.
type CatalogStore interface {
	Load() (*Catalog, bool)
	Save(*Catalog) error
	WithLock(ctx context.Context, fn func() error) error
}

type memoryStore struct {
	buildMu sync.Mutex
	mu      sync.Mutex
	cat     *Catalog
}

// NewMemoryStore returns a process-local CatalogStore for tests and callers
// that must not touch the disk cache.
func NewMemoryStore() CatalogStore { return &memoryStore{} }

func (s *memoryStore) Load() (*Catalog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cat, s.cat != nil
}

func (s *memoryStore) Save(cat *Catalog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cat = cat
	return nil
}

func (s *memoryStore) WithLock(_ context.Context, fn func() error) error {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	return fn()
}

type diskStore struct {
	dir string
}

// NewDiskStore returns the shared on-disk CatalogStore under the ccx cache
// dir; its lock serializes catalog builds across concurrent CLI invocations.
func NewDiskStore() (CatalogStore, error) {
	dir, err := cache.Dir("exec")
	if err != nil {
		return nil, fmt.Errorf("resolve exec cache dir: %w", err)
	}
	return &diskStore{dir: dir}, nil
}

func (s *diskStore) path() string { return filepath.Join(s.dir, "catalog.json") }

func (s *diskStore) Load() (*Catalog, bool) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return nil, false
	}
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, false
	}
	return &cat, true
}

func (s *diskStore) Save(cat *Catalog) error {
	data, err := json.Marshal(cat)
	if err != nil {
		return fmt.Errorf("encode catalog: %w", err)
	}
	if err := cache.Store(s.path(), data, 0o600); err != nil {
		return fmt.Errorf("store catalog: %w", err)
	}
	return nil
}

func (s *diskStore) WithLock(ctx context.Context, fn func() error) error {
	return cache.WithLock(ctx, s.dir, "catalog", fn)
}

// serverTimeout bounds one server's connect-and-list during a catalog build.
const serverTimeout = 15 * time.Second

// buildConcurrency bounds how many servers a catalog build probes at once.
const buildConcurrency = 4

// resolveCatalog returns the store's catalog when its hash matches inv (zero
// server spawns), otherwise builds and saves one under the store lock.
func resolveCatalog(ctx context.Context, store CatalogStore, inv Inventory, connect connectFunc) (*Catalog, error) {
	if cat, ok := store.Load(); ok && cat.Hash == inv.Hash {
		return cat, nil
	}
	var cat *Catalog
	err := store.WithLock(ctx, func() error {
		if loaded, ok := store.Load(); ok && loaded.Hash == inv.Hash {
			cat = loaded
			return nil
		}
		cat = buildCatalog(ctx, inv, connect, serverTimeout)
		return store.Save(cat)
	})
	if err != nil {
		return nil, fmt.Errorf("resolve catalog: %w", err)
	}
	return cat, nil
}

// buildCatalog probes every inventory server concurrently — connect, list
// tools, close — recording any failure as that server's Note so one bad server
// never blocks the rest. The catalog needs schemas, not live sessions.
func buildCatalog(ctx context.Context, inv Inventory, connect connectFunc, timeout time.Duration) *Catalog {
	cat := &Catalog{Hash: inv.Hash, Built: time.Now(), Servers: make([]ServerCatalog, len(inv.Servers))}
	var g errgroup.Group
	g.SetLimit(buildConcurrency)
	for i, spec := range inv.Servers {
		g.Go(func() error {
			cat.Servers[i] = probeServer(ctx, spec, connect, timeout)
			return nil
		})
	}
	_ = g.Wait()
	return cat
}

func probeServer(ctx context.Context, spec ServerSpec, connect connectFunc, timeout time.Duration) ServerCatalog {
	sc := ServerCatalog{Spec: spec}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, err := connect(ctx, spec)
	if err != nil {
		sc.Note = fmt.Sprintf("not reflected: connect %s: %v", spec.Name, err)
		return sc
	}
	defer func() { _ = session.Close() }()
	var raw []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			sc.Note = fmt.Sprintf("not reflected: list tools on %s: %v", spec.Name, err)
			return sc
		}
		raw = append(raw, tool)
	}
	sc.Tools = make([]ToolInfo, len(raw))
	for i, tool := range raw {
		sc.Tools[i] = toolInfo(spec, tool)
	}
	sc.Note = classify(spec, raw)
	return sc
}

// classify flags a stdio server that may depend on live session state: unless
// its tools are all read-only or predominantly open-world, every reflected
// call reaches a fresh instance rather than the session Claude is attached to,
// and the note tells the user how to opt out.
func classify(spec ServerSpec, tools []*mcp.Tool) string {
	if spec.URL != "" || len(tools) == 0 {
		return ""
	}
	readOnly, openWorld := 0, 0
	for _, t := range tools {
		if t.Annotations == nil {
			continue
		}
		if t.Annotations.ReadOnlyHint {
			readOnly++
		}
		if t.Annotations.OpenWorldHint != nil && *t.Annotations.OpenWorldHint {
			openWorld++
		}
	}
	if readOnly == len(tools) || openWorld*2 > len(tools) {
		return ""
	}
	return fmt.Sprintf("reflected %s as a fresh instance — if it needs live session state, set CCX_EXEC_MCP_DENY=%s", spec.Name, spec.Name)
}

func toolInfo(spec ServerSpec, tool *mcp.Tool) ToolInfo {
	info := ToolInfo{
		Server:   spec.Name,
		Tool:     tool.Name,
		FuncName: spec.Prefix + "_" + sanitizeIdent(tool.Name),
		Params:   schemaParams(tool.InputSchema),
		Summary:  firstSentence(tool.Description),
	}
	if tool.Annotations != nil {
		info.ReadOnly = tool.Annotations.ReadOnlyHint
		info.Destructive = tool.Annotations.DestructiveHint
	}
	return info
}

// schemaParams lists a tool's input property names, required first, each group
// sorted so the signature is stable across builds.
func schemaParams(schema any) []string {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	required := make(map[string]bool, len(s.Required))
	var params []string
	for _, name := range slices.Sorted(slices.Values(s.Required)) {
		if _, ok := s.Properties[name]; ok {
			required[name] = true
			params = append(params, name)
		}
	}
	var optional []string
	for name := range s.Properties {
		if !required[name] {
			optional = append(optional, name)
		}
	}
	return append(params, slices.Sorted(slices.Values(optional))...)
}

// summaryMax caps a reflected tool's one-line summary (in runes).
const summaryMax = 120

func firstSentence(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.Index(desc, "\n"); i >= 0 {
		desc = strings.TrimSpace(desc[:i])
	}
	if i := strings.Index(desc, ". "); i >= 0 {
		desc = desc[:i+1]
	}
	if r := []rune(desc); len(r) > summaryMax {
		desc = strings.TrimSpace(string(r[:summaryMax-1])) + "…"
	}
	return desc
}
