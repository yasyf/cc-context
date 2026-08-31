// Package workspace resolves the project root ops answer against: the root an
// MCP client declared, else the process working directory.
package workspace

import (
	"context"
	"os"
	"sync"
)

var (
	mu     sync.RWMutex
	pinned string
)

// rootKey keys the declared root one request carries.
type rootKey struct{}

// SetRoot pins the project root every op resolves against. The MCP server
// calls it with the root its client declared; an empty dir clears the pin.
func SetRoot(dir string) {
	mu.Lock()
	defer mu.Unlock()
	pinned = dir
}

// WithRoot returns a copy of ctx carrying dir as the declared project root for
// everything the request reaches, an empty dir meaning none was declared. The
// MCP server snapshots the pin into the request context once, so the ops and
// the root header of one call answer from the same tree even when a concurrent
// call re-pins mid-flight.
func WithRoot(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, rootKey{}, dir)
}

// Root returns the pinned project root, falling back to the process working
// directory when no client has declared one.
func Root() (string, error) {
	return RootFrom(context.Background())
}

// RootFrom returns the project root ctx carries, falling back to the pin when
// ctx carries none and then to the process working directory when nothing is
// declared.
func RootFrom(ctx context.Context) (string, error) {
	if dir := DeclaredFrom(ctx); dir != "" {
		return dir, nil
	}
	return os.Getwd()
}

// Declared returns the pinned project root, empty when no client has declared
// one and ops resolve against the process working directory.
func Declared() string {
	mu.RLock()
	defer mu.RUnlock()
	return pinned
}

// DeclaredFrom returns the declared project root ctx carries, falling back to
// the pin when ctx carries none. Empty means no client declared one.
func DeclaredFrom(ctx context.Context) string {
	if dir, ok := ctx.Value(rootKey{}).(string); ok {
		return dir
	}
	return Declared()
}
