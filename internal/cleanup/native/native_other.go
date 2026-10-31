//go:build !darwin

package native

import (
	"context"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// FSEventsSampler has nothing to sample off darwin, where no fseventsd runs.
type FSEventsSampler struct{}

// NewFSEventsSampler returns a sampler whose every Sample reports
// cleanup.ErrUnsupported.
func NewFSEventsSampler() *FSEventsSampler { return &FSEventsSampler{} }

// Sample reports cleanup.ErrUnsupported.
func (*FSEventsSampler) Sample(_ context.Context) (time.Duration, error) {
	return 0, cleanup.ErrUnsupported
}

// Guard reports cleanup.ErrUnsupported, which authorizes nothing.
func Guard(ctx context.Context, path string) error {
	_, err := GuardRoots(ctx, cleanup.GuardRoot{Path: path, Retiring: cleanup.RetiringFrom(ctx)})
	return err
}

// Band has no darwin band to switch off darwin.
type Band struct{}

var _ cleanup.Band = Band{}

// Background reports cleanup.ErrUnsupported and leaves the process's
// scheduling as it was.
func (Band) Background() error { return cleanup.ErrUnsupported }

// Foreground reports cleanup.ErrUnsupported and leaves the process's
// scheduling as it was.
func (Band) Foreground() error { return cleanup.ErrUnsupported }

// ProcessUniqueID reports cleanup.ErrUnsupported: no process is identified
// off darwin.
func ProcessUniqueID(int) (uint64, error) { return 0, cleanup.ErrUnsupported }

// Identify reports cleanup.ErrUnsupported: no process is identified off
// darwin.
func Identify(int) (cleanup.ProcessID, error) { return cleanup.ProcessID{}, cleanup.ErrUnsupported }

// ParentCommand reports cleanup.ErrUnsupported: no process is inspected off
// darwin.
func ParentCommand(int) (int, []string, error) { return 0, nil, cleanup.ErrUnsupported }

// GuardRoots reports that native activity inspection is unavailable on this platform.
func GuardRoots(_ context.Context, first cleanup.GuardRoot, _ ...cleanup.GuardRoot) (string, error) {
	return first.Path, cleanup.ErrUnsupported
}
