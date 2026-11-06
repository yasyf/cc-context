package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/render"
)

func laneNames(ctx context.Context, prefix string) (cleanup.Names, error) {
	home, err := render.Home(ctx)
	if err != nil {
		return cleanup.Names{}, fmt.Errorf("%s: resolve home directory: %w", prefix, err)
	}
	return cleanup.LayoutUnder(home).Names(), nil
}

func laneUsed(ctx context.Context, prefix, path string) (cleanup.Use, bool, error) {
	names, err := laneNames(ctx, prefix)
	if err != nil {
		return cleanup.Use{}, false, err
	}
	use, used, err := names.Used(path)
	if err != nil {
		return cleanup.Use{}, false, fmt.Errorf("%s: %w", prefix, err)
	}
	return use, used, nil
}

// refuseUsedLane refuses path when ccx has cut or removed it, or another path
// with its lane name, before: a removed lane deletes in place whenever the
// cleanup queue reaches it, so its name is never cut again.
func refuseUsedLane(ctx context.Context, prefix, name, path string) error {
	use, used, err := laneUsed(ctx, prefix, path)
	if err != nil || !used {
		return err
	}
	fresh, err := freshLaneName(ctx, prefix, name, path)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s: lane %s was already used by %s at %s (%s), and ccx never cuts a lane name twice — pick a new name, such as %s",
		prefix, cleanup.LaneName(path), use.By, use.At.Local().Format(time.RFC3339), use.Path, fresh)
}

func freshLaneName(ctx context.Context, prefix, name, path string) (string, error) {
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		free, err := laneFree(ctx, prefix, path+suffix)
		if err != nil || free {
			return name + suffix, err
		}
	}
}

func laneFree(ctx context.Context, prefix, path string) (bool, error) {
	if _, used, err := laneUsed(ctx, prefix, path); err != nil || used {
		return false, err
	}
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", prefix, err)
	}
	return false, nil
}

func recordLane(ctx context.Context, prefix, path string) error {
	names, err := laneNames(ctx, prefix)
	if err != nil {
		return err
	}
	if err := names.Record(path, prefix, time.Now().UTC()); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}
