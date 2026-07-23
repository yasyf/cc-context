package web

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// stderrTail bounds how much driver stderr is retained for crash reports.
const stderrTail = 8 << 10

// driverErr wraps a driver failure with the context's verdict when the deadline
// (or caller) killed it, else the stderr tail when there is one.
func driverErr(ctx context.Context, op string, err error, stderr *tailBuffer) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", op, ctxErr)
	}
	if tail := stderr.String(); tail != "" {
		return fmt.Errorf("%s: %w: %s", op, err, tail)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// tailBuffer keeps the last stderrTail bytes a driver wrote, for crash reports.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - stderrTail; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
