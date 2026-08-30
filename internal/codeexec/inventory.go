package codeexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/workspace"
)

// inventoryTTL is how long an Engine trusts one `claude mcp list` probe before
// re-running it. Server sets change rarely; the CCX_EXEC_MCP=refresh hatch
// covers additions inside the window.
const inventoryTTL = 15 * time.Minute

// InventoryStore caches one discovery probe across Engine instances, keyed by
// the project root it probed. A corrupt or missing record reads as a miss.
type InventoryStore interface {
	Load() (Inventory, time.Time, bool)
	Save(Inventory, time.Time) error
}

// inventoryEnvelope is the persisted record: the wall-clock probe time, the raw
// ALLOW/DENY filter env the probe was pre-filtered under, and the inventory it
// produced.
type inventoryEnvelope struct {
	Probed    time.Time `json:"probed"`
	Allow     string    `json:"allow"`
	Deny      string    `json:"deny"`
	Inventory Inventory `json:"inventory"`
}

// mcpFilterEnv reads the raw ALLOW/DENY values that pre-filter a probe. A change
// to either must invalidate the cache — DENY is a safety control.
func mcpFilterEnv() (allow, deny string) {
	return os.Getenv("CCX_EXEC_MCP_ALLOW"), os.Getenv("CCX_EXEC_MCP_DENY")
}

// valid reports whether env is a usable record: probed at a real time and under
// the ALLOW/DENY filter still in effect. A zero probe time (an absent or
// half-written envelope) or a filter change reads as a miss.
func (env *inventoryEnvelope) valid() bool {
	if env.Probed.IsZero() {
		return false
	}
	allow, deny := mcpFilterEnv()
	return env.Allow == allow && env.Deny == deny
}

type memoryInventoryStore struct {
	mu  sync.Mutex
	env *inventoryEnvelope
}

// NewMemoryInventoryStore returns a process-local InventoryStore for tests and
// the resident facade, which caches within one process lifetime.
func NewMemoryInventoryStore() InventoryStore { return &memoryInventoryStore{} }

func (s *memoryInventoryStore) Load() (Inventory, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.env == nil || !s.env.valid() {
		return Inventory{}, time.Time{}, false
	}
	return s.env.Inventory, s.env.Probed, true
}

func (s *memoryInventoryStore) Save(inv Inventory, probed time.Time) error {
	allow, deny := mcpFilterEnv()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.env = &inventoryEnvelope{Probed: probed, Allow: allow, Deny: deny, Inventory: inv}
	return nil
}

type diskInventoryStore struct {
	dir  string
	root func() (string, error)
}

// NewDiskInventoryStore returns the on-disk InventoryStore under the shared
// exec cache dir. `claude mcp list` output is project-scoped, so the record is
// keyed by the project root, resolved per call so a root switch reads its own
// record rather than the previous root's.
func NewDiskInventoryStore() (InventoryStore, error) {
	dir, err := cache.Dir("exec")
	if err != nil {
		return nil, fmt.Errorf("resolve exec cache dir: %w", err)
	}
	return &diskInventoryStore{dir: dir, root: workspace.Root}, nil
}

// newDiskInventoryStore pins the record file to one root so tests can key
// arbitrary directories without changing the process working directory.
func newDiskInventoryStore(dir, root string) *diskInventoryStore {
	return &diskInventoryStore{dir: dir, root: func() (string, error) { return root, nil }}
}

func (s *diskInventoryStore) path() (string, error) {
	root, err := s.root()
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(s.dir, "inventory-"+hex.EncodeToString(sum[:])[:16]+".json"), nil
}

func (s *diskInventoryStore) Load() (Inventory, time.Time, bool) {
	path, err := s.path()
	if err != nil {
		return Inventory{}, time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Inventory{}, time.Time{}, false
	}
	var env inventoryEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Inventory{}, time.Time{}, false
	}
	if !env.valid() {
		return Inventory{}, time.Time{}, false
	}
	return env.Inventory, env.Probed, true
}

func (s *diskInventoryStore) Save(inv Inventory, probed time.Time) error {
	path, err := s.path()
	if err != nil {
		return err
	}
	allow, deny := mcpFilterEnv()
	data, err := json.Marshal(inventoryEnvelope{Probed: probed, Allow: allow, Deny: deny, Inventory: inv})
	if err != nil {
		return fmt.Errorf("encode inventory: %w", err)
	}
	if err := cache.Store(path, data, 0o600); err != nil {
		return fmt.Errorf("store inventory: %w", err)
	}
	return nil
}
