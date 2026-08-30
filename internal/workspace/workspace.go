// Package workspace resolves the project root ops answer against: the root an
// MCP client declared, else the process working directory.
package workspace

import (
	"os"
	"sync"
)

var (
	mu     sync.RWMutex
	pinned string
)

// SetRoot pins the project root every op resolves against. The MCP server
// calls it with the root its client declared; an empty dir clears the pin.
func SetRoot(dir string) {
	mu.Lock()
	defer mu.Unlock()
	pinned = dir
}

// Root returns the pinned project root, falling back to the process working
// directory when no client has declared one.
func Root() (string, error) {
	mu.RLock()
	dir := pinned
	mu.RUnlock()
	if dir != "" {
		return dir, nil
	}
	return os.Getwd()
}
