package cli

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

func replayPrefetch(ctx context.Context, dir render.Dir, prefix, onto, from, head string) {
	if err := replayFetchBlobs(ctx, dir, onto, from, head); err != nil {
		slog.Warn(prefix+": batch-fetch the replay's blobs from the promisor remote; the replay fetches them commit by commit instead", "onto", onto, "range", from+".."+head, "err", err)
	}
}

func replayFetchBlobs(ctx context.Context, dir render.Dir, onto, from, head string) error {
	remote, err := replayPromisor(ctx, dir)
	if err != nil || remote == "" {
		return err
	}
	commits, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", from + ".." + head})
	if err != nil {
		return err
	}
	if commits == "" {
		return nil
	}
	out, err := render.RunCLIStdin(ctx, dir, "git", []string{"diff-tree", "--stdin", "-r", "--root", "--no-renames", "--no-commit-id", "-z"}, []byte(commits))
	if err != nil {
		return err
	}
	touched, err := replayRawBlobs(out)
	if err != nil || len(touched) == 0 {
		return err
	}
	base, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", from, head})
	if err != nil {
		return err
	}
	out, err = render.RunCLI(ctx, dir, "git", []string{"diff-tree", "-r", "--no-renames", "-z", strings.TrimSpace(base), onto})
	if err != nil {
		return err
	}
	upstream, err := replayRawBlobs(out)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for path, oids := range touched {
		for _, oid := range slices.Concat(oids, upstream[path]) {
			want[oid] = true
		}
	}
	missing, err := replayMissing(ctx, dir, slices.Sorted(maps.Keys(want)))
	if err != nil || len(missing) == 0 {
		return err
	}
	_, err = render.RunCLIStdin(ctx, dir, "git", []string{"-c", "fetch.negotiationAlgorithm=noop", "fetch", remote, "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", "--stdin"}, []byte(strings.Join(missing, "\n")+"\n"))
	return err
}

func replayPromisor(ctx context.Context, dir render.Dir) (string, error) {
	out, err := render.RunCLIAllowExit(ctx, dir, "git", []string{"config", "-z", "--type=bool", "--get-regexp", `^remote\..+\.promisor$`}, 1)
	if err != nil {
		return "", err
	}
	for entry := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		remote := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".promisor")
		if value != "true" {
			continue
		}
		filter, err := render.RunCLIAllowExit(ctx, dir, "git", []string{"config", "--get", "remote." + remote + ".partialclonefilter"}, 1)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(strings.TrimSpace(filter), "blob:") {
			return remote, nil
		}
	}
	return "", nil
}

func replayRawBlobs(out string) (map[string][]string, error) {
	blobs := map[string][]string{}
	if out == "" {
		return blobs, nil
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("read git diff-tree -z output as raw entry and path pairs: %d fields", len(fields))
	}
	for i := 0; i < len(fields); i += 2 {
		meta, ok := strings.CutPrefix(fields[i], ":")
		entry := strings.Fields(meta)
		if !ok || len(entry) != 5 {
			return nil, fmt.Errorf("read git diff-tree raw entry %q", fields[i])
		}
		for _, side := range [][2]string{{entry[0], entry[2]}, {entry[1], entry[3]}} {
			if side[0] != "160000" && strings.Trim(side[1], "0") != "" {
				blobs[fields[i+1]] = append(blobs[fields[i+1]], side[1])
			}
		}
	}
	return blobs, nil
}

func replayMissing(ctx context.Context, dir render.Dir, oids []string) ([]string, error) {
	out, err := render.RunCLIStdinEnv(ctx, dir, "git", []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, []byte(strings.Join(oids, "\n")+"\n"), []string{"GIT_NO_LAZY_FETCH=1"})
	if err != nil {
		return nil, err
	}
	var missing []string
	for line := range strings.Lines(out) {
		if oid, ok := strings.CutSuffix(strings.TrimSuffix(line, "\n"), " missing"); ok {
			missing = append(missing, oid)
		}
	}
	return missing, nil
}
