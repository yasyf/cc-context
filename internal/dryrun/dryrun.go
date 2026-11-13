// Package dryrun keeps a dry run's writes to stack metadata files in memory,
// where the rest of the run reads them back, so a dry run plans from the state
// its real run would record and leaves every file as it found it.
package dryrun

import (
	"context"
	"sync"
)

type key struct{}

type files struct {
	mu    sync.Mutex
	bytes map[string][]byte
}

// With marks ctx as a dry run, holding its metadata writes in memory from here on.
func With(ctx context.Context) context.Context {
	return context.WithValue(ctx, key{}, &files{bytes: map[string][]byte{}})
}

// Active reports whether ctx belongs to a dry run.
func Active(ctx context.Context) bool {
	_, ok := ctx.Value(key{}).(*files)
	return ok
}

// Read returns the content the dry run wrote to path, and whether it wrote any.
func Read(ctx context.Context, path string) ([]byte, bool) {
	f, ok := ctx.Value(key{}).(*files)
	if !ok {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.bytes[path]
	return data, ok
}

// Write keeps data as path's content for the rest of the dry run.
func Write(ctx context.Context, path string, data []byte) {
	f := ctx.Value(key{}).(*files)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bytes[path] = data
}
