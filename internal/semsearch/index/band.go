package index

import (
	"context"
	"log/slog"
	"sync"
)

// Band switches the process between the background scheduling band and the
// default one.
type Band interface {
	Background() error
	Foreground() error
}

type (
	bandKey     struct{}
	loadBandKey struct{}
)

// WithBand has Load do its indexing in band's background state, returning to the
// foreground before it hands the index back. A Load with nothing to index never
// leaves the foreground.
func WithBand(ctx context.Context, band Band) context.Context {
	return context.WithValue(ctx, bandKey{}, band)
}

type loadBand struct {
	band    Band
	once    sync.Once
	entered bool
}

func withLoadBand(ctx context.Context) (context.Context, func()) {
	band, ok := ctx.Value(bandKey{}).(Band)
	if !ok {
		return ctx, func() {}
	}
	lb := &loadBand{band: band}
	return context.WithValue(ctx, loadBandKey{}, lb), lb.leave
}

func enterBackground(ctx context.Context) {
	if lb, ok := ctx.Value(loadBandKey{}).(*loadBand); ok {
		lb.once.Do(lb.enter)
	}
}

func (b *loadBand) enter() {
	if err := b.band.Background(); err != nil {
		slog.Warn("semsearch: enter the background band", "err", err)
		return
	}
	b.entered = true
}

func (b *loadBand) leave() {
	b.once.Do(func() {})
	if !b.entered {
		return
	}
	if err := b.band.Foreground(); err != nil {
		slog.Warn("semsearch: leave the background band", "err", err)
	}
}
