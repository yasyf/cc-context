package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	cleanupRateCapKey     = "ccx.cleanupRateCap"
	cleanupBusyRateKey    = "ccx.cleanupBusyRate"
	cleanupBusyWorkersKey = "ccx.cleanupBusyWorkers"
	cleanupQuietRootKey   = "ccx.cleanupQuietRoot"
	cleanupMoveRootKey    = "ccx.cleanupMoveRoot"
	cleanupMoveEveryKey   = "ccx.cleanupMoveEvery"

	cleanupMoveEveryDefault = 5 * time.Second
)

func parseCleanupRateCaps(values string) ([]cleanup.RateCap, error) {
	var caps []cleanup.RateCap
	for value := range configValues(values) {
		at := strings.LastIndexByte(value, '=')
		if at < 0 {
			return nil, fmt.Errorf("%s must be <absolute-root>=<entries-per-second>, got %q", cleanupRateCapKey, value)
		}
		root := value[:at]
		rate, err := strconv.Atoi(value[at+1:])
		if err != nil || rate <= 0 || !filepath.IsAbs(root) {
			return nil, fmt.Errorf("%s must be <absolute-root>=<entries-per-second>, got %q", cleanupRateCapKey, value)
		}
		root, err = resolvedRoot(root)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cleanupRateCapKey, err)
		}
		caps = append(caps, cleanup.RateCap{Root: root, Rate: rate})
	}
	return caps, nil
}

func parseCleanupBusyDeletes(rate, workers, quiet, move, every string) (cleanup.BusyDeletes, error) {
	busy := cleanup.BusyDeletes{Workers: 1}
	if workers = strings.TrimSpace(workers); workers != "" {
		parsed, err := strconv.Atoi(workers)
		if err != nil || parsed <= 0 {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be a positive count, got %q", cleanupBusyWorkersKey, workers)
		}
		busy.Workers = parsed
	}
	if rate = strings.TrimSpace(rate); rate != "" {
		parsed, err := strconv.Atoi(rate)
		if err != nil || parsed <= 0 {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be entries per second, got %q", cleanupBusyRateKey, rate)
		}
		busy.Rate = parsed
	}
	for root := range configValues(quiet) {
		if !filepath.IsAbs(root) {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be an absolute directory, got %q", cleanupQuietRootKey, root)
		}
		resolved, err := resolvedRoot(root)
		if err != nil {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s: %w", cleanupQuietRootKey, err)
		}
		busy.Quiet = append(busy.Quiet, resolved)
	}
	for root := range configValues(move) {
		if !filepath.IsAbs(root) {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be an absolute directory, got %q", cleanupMoveRootKey, root)
		}
		resolved, err := resolvedRoot(root)
		if err != nil {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s: %w", cleanupMoveRootKey, err)
		}
		busy.Move = append(busy.Move, resolved)
	}
	busy.MoveEvery = cleanupMoveEveryDefault
	if every = strings.TrimSpace(every); every != "" {
		parsed, err := time.ParseDuration(every)
		if err != nil || parsed <= 0 {
			return cleanup.BusyDeletes{}, fmt.Errorf("%s must be a positive duration, got %q", cleanupMoveEveryKey, every)
		}
		busy.MoveEvery = parsed
	}
	return busy, nil
}

func configValues(values string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for value := range strings.Lines(values) {
			value = strings.TrimSpace(value)
			if value != "" && !yield(value) {
				return
			}
		}
	}
}

func resolvedRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) {
		return filepath.Clean(root), nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", root, err)
	}
	return resolved, nil
}
