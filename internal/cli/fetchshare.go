package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	sharedFetchDir     = "ccx-fetch"
	sharedFetchMaxWait = 10 * time.Minute
)

// sharedFetch runs fetch once for every worktree of the clone asking for
// remote's branch at the same time: callers queue on a lock in the git common
// dir, and one whose turn follows a fetch begun no more than maxAge before it
// asked takes that result instead. It reports whether this caller fetched.
func sharedFetch(ctx context.Context, dir render.Dir, prefix, remote, branch string, maxAge time.Duration, fetch func() error) (bool, error) {
	since := time.Now().Add(-maxAge)
	commonDir, err := sharedFetchCommonDir(ctx, dir, prefix)
	if err != nil {
		return false, err
	}
	stamp := filepath.Join(commonDir, sharedFetchDir, fmt.Sprintf("%x", sha256.Sum256([]byte(remote+"\x00"+branch))))
	if maxAge > 0 && sharedFetchCovers(stamp, since) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return false, fmt.Errorf("%s: %w", prefix, err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, sharedFetchMaxWait)
	defer cancel()
	lock, err := durable.AcquireLock(lockCtx, stamp+".lock")
	if err != nil {
		return false, fmt.Errorf("%s: wait for the shared fetch of %s/%s: %w", prefix, remote, branch, err)
	}
	defer func() { _ = lock.Close() }()
	if sharedFetchCovers(stamp, since) {
		return false, nil
	}
	started := time.Now()
	if err := fetch(); err != nil {
		return false, err
	}
	if err := os.WriteFile(stamp, []byte(started.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil { //nolint:gosec // a ccx-owned file in the git common dir
		return true, fmt.Errorf("%s: record the shared fetch of %s/%s: %w", prefix, remote, branch, err)
	}
	return true, nil
}

func sharedFetchCommonDir(ctx context.Context, dir render.Dir, prefix string) (string, error) {
	if gitCommonDirOverridden(ctx) {
		return gtCommonDir(ctx, dir, prefix)
	}
	ck, err := vcs.ResolveCheckout(string(dir))
	if err != nil {
		return "", fmt.Errorf("%s: %w", prefix, err)
	}
	if ck.CommonDir == "" {
		return gtCommonDir(ctx, dir, prefix)
	}
	return ck.CommonDir, nil
}

func sharedFetchCovers(stamp string, since time.Time) bool {
	data, err := os.ReadFile(stamp) //nolint:gosec // a ccx-owned file in the git common dir
	if err != nil {
		return false
	}
	began, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	return err == nil && !began.Before(since)
}

type vcsFetchOpts struct {
	remote string
	maxAge time.Duration
}

func newVcsFetchCmd() *cobra.Command {
	var o vcsFetchOpts
	cmd := &cobra.Command{
		Use:   "fetch <branch>...",
		Short: "Fetch remote branches into their remote-tracking refs, one fetch shared by every worktree asking at once",
		Long: `Fetch remote branches into their remote-tracking refs, one fetch shared by every worktree asking at once.

Every worktree of a clone shares one object store and one set of remote-tracking
refs, so one fetch serves them all. Fetch queues each caller on a lock in the git
common dir: the first runs git fetch, and every caller that was already waiting
when that fetch began takes its result instead of fetching again. A caller never
gets a ref older than its own request unless --max-age allows it: with
--max-age 30s, a fetch of the branch begun within the last 30 seconds answers
without a round trip.

Each branch lands in refs/remotes/<remote>/<branch>, and fetch prints one line
per branch: the ref, its sha, and "fetched" when this call ran the fetch or
"shared" when another's answered it. ccx vcs ship, ccx vcs stack submit and the
other commands that refresh the remote trunk share their fetch the same way.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsFetch(cmd, o, args)
		},
	}
	cmd.Flags().StringVar(&o.remote, "remote", "origin", "remote to fetch from")
	cmd.Flags().DurationVar(&o.maxAge, "max-age", 0, "accept a fetch of the branch begun this long before the call")
	return cmd
}

func runVcsFetch(cmd *cobra.Command, o vcsFetchOpts, branches []string) error {
	ctx := cmd.Context()
	ck, err := vcs.ResolveCheckout(workingDir(ctx))
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if ck.Kind == vcs.None {
		return errors.New("fetch: no git repository in the working directory")
	}
	dir := render.Dir(ck.Root)
	for _, branch := range branches {
		ref := "refs/remotes/" + o.remote + "/" + branch
		fetched, err := sharedFetch(ctx, dir, "fetch", o.remote, branch, o.maxAge, func() error {
			return gitFetch(ctx, dir, "--quiet", "--no-tags", "--no-write-fetch-head", o.remote, "+refs/heads/"+branch+":"+ref)
		})
		if err != nil {
			return fmt.Errorf("fetch: git fetch %s %s: %w", o.remote, branch, err)
		}
		sha, err := gitRevParse(ctx, dir, "fetch", ref)
		if err != nil {
			return err
		}
		how := "shared"
		if fetched {
			how = "fetched"
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", ref, sha, how); err != nil {
			return err
		}
	}
	return nil
}
