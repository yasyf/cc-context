package cleanup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Names is the durable index of every worktree path ccx has cut or queued for
// removal, and of each path's lane name: its pool directory's name and its own,
// so the same lane minted under another worktree root is the same name. An
// entry is never removed, so no path or lane name is ever cut twice. Each key
// is one file named for its hash, so a lookup is one read and never a walk.
type Names struct {
	dir string
}

// Use is the first recorded use of a worktree path or of its lane name.
type Use struct {
	Path string    `json:"path"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

// Names is the layout's index of used worktree paths and lane names.
func (l Layout) Names() Names { return Names{dir: filepath.Join(l.Root, "names")} }

// LaneName is path's lane name: the name of its pool directory and its own.
func LaneName(path string) string {
	return filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path)
}

// Record notes that by used path at at. A path or lane name already recorded
// keeps its first use.
func (n Names) Record(path, by string, at time.Time) error {
	if err := os.MkdirAll(n.dir, 0o700); err != nil {
		return fmt.Errorf("cleanup: make the name index: %w", err)
	}
	root, err := os.OpenRoot(n.dir)
	if err != nil {
		return fmt.Errorf("cleanup: open the name index: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := json.Marshal(Use{Path: path, By: by, At: at})
	if err != nil {
		return fmt.Errorf("cleanup: encode the use of %s: %w", path, err)
	}
	for _, key := range nameKeys(path) {
		if err := publish(root, key, data); err != nil {
			return fmt.Errorf("cleanup: record %s: %w", path, err)
		}
	}
	return nil
}

// Used returns the first recorded use of path or of its lane name.
func (n Names) Used(path string) (Use, bool, error) {
	root, err := os.OpenRoot(n.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Use{}, false, nil
	}
	if err != nil {
		return Use{}, false, fmt.Errorf("cleanup: open the name index: %w", err)
	}
	defer func() { _ = root.Close() }()
	for _, key := range nameKeys(path) {
		data, err := root.ReadFile(key)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Use{}, false, fmt.Errorf("cleanup: look up %s: %w", path, err)
		}
		var use Use
		if err := json.Unmarshal(data, &use); err != nil {
			return Use{}, false, fmt.Errorf("cleanup: decode the use of %s: %w", path, err)
		}
		return use, true, nil
	}
	return Use{}, false, nil
}

func nameKeys(path string) []string {
	keys := make([]string, 0, 2)
	for _, key := range []string{"path\x00" + path, "lane\x00" + LaneName(path)} {
		sum := sha256.Sum256([]byte(key))
		name := hex.EncodeToString(sum[:])
		keys = append(keys, filepath.Join(name[:2], name))
	}
	return keys
}

func publish(root *os.Root, key string, data []byte) error {
	if _, err := root.Lstat(key); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		return err
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	staged := key + "." + hex.EncodeToString(suffix[:]) + ".tmp"
	if err := root.WriteFile(staged, data, 0o600); err != nil {
		return err
	}
	defer func() { _ = root.Remove(staged) }()
	if err := root.Link(staged, key); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
