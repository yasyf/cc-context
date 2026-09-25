package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type stackNewOpts struct {
	parent     string
	path       string
	published  bool
	sparse     bool
	noCheckout bool
}

type stackSparse struct {
	patterns []byte
	cone     bool
}

func runStackNew(cmd *cobra.Command, name string, options stackNewOpts) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "stack new", workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack new: this repository is not on the graphite lane, and a stack is Graphite's — run gt init, or cut a plain working copy with ccx vcs worktree add")
	}
	if (options.sparse || options.noCheckout) && l.checkout.Kind != vcs.Git {
		return errors.New("stack new: sparse and no-checkout creation require a Git checkout")
	}
	parent := options.parent
	if parent == "" {
		parent, err = gitCurrentBranch(ctx, l.dir(), "stack new")
		if err != nil {
			return err
		}
	}
	if parent == "" {
		return errors.New("stack new: HEAD is detached here, so there is no branch to stack on — check one out, or name it with --parent")
	}
	path, err := stackNewPath(ctx, l.checkout, name, options.path)
	if err != nil {
		return err
	}
	var sparse stackSparse
	if options.sparse {
		sparse, err = stackReadSparse(ctx, l.dir())
		if err != nil {
			return err
		}
	}
	start := parent
	var receipt *stackPublication
	var common string
	if options.published {
		common, err = gtCommonDir(ctx, l.dir(), "stack new")
		if err != nil {
			return err
		}
		receipt, err = stackReadPublication(ctx, l.dir(), parent)
		if err != nil {
			return err
		}
		if receipt == nil {
			return fmt.Errorf("stack new: %s has no publication receipt; publish it first", parent)
		}
		if err := stackVerifyNewParent(ctx, l.dir(), common, receipt); err != nil {
			return err
		}
		start = receipt.Head
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	args := []string{"worktree", "add"}
	if options.sparse || options.noCheckout {
		args = append(args, "--no-checkout")
	}
	args = append(args, "-b", name, path, start)
	if _, err := render.RunCLI(ctx, l.dir(), "git", args); err != nil {
		return fmt.Errorf("stack new: create %s: %w", path, err)
	}
	created := render.Dir(path)
	finish := func() error {
		if options.sparse {
			if err := stackWriteSparse(ctx, created, sparse); err != nil {
				return err
			}
		}
		if receipt == nil {
			return stackFormLane(ctx, cmd.ErrOrStderr(), l, created, name, parent)
		}
		if l.checkout.Kind == vcs.JJ {
			if err := stackColocateJJ(ctx, created, name); err != nil {
				return err
			}
		}
		if err := stackTrackPublished(ctx, created, cmd.ErrOrStderr(), common, name, receipt); err != nil {
			return err
		}
		if err := stackVerifyNewParent(ctx, l.dir(), common, receipt); err != nil {
			return err
		}
		return nil
	}
	if err := finish(); err != nil {
		if receipt != nil {
			err = errors.Join(err, gtmeta.Forget(ctx, common, []string{name}))
		}
		return errors.Join(err, stackUnwindLane(ctx, l.dir(), path, name))
	}
	cmd.Println(strings.Join([]string{"cut " + name + " onto " + parent, path}, shipSep))
	return nil
}

func stackNewPath(ctx context.Context, checkout vcs.Checkout, name, requested string) (string, error) {
	path, err := mintWorktreePath(ctx, "stack new", checkout, name)
	if err != nil {
		return "", err
	}
	if requested != "" {
		if !filepath.IsAbs(requested) {
			requested = filepath.Join(workingDir(ctx), requested)
		}
		path, err = filepath.Abs(requested)
		if err != nil {
			return "", err
		}
		ancestor := filepath.Dir(path)
		tail := filepath.Base(path)
		for {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err == nil {
				path = filepath.Join(resolved, tail)
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			tail = filepath.Join(filepath.Base(ancestor), tail)
			ancestor = filepath.Dir(ancestor)
		}
	}
	for _, root := range []string{checkout.Root, checkout.MainRoot} {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("stack new: destination %s is inside checkout %s", path, root)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("stack new: destination %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

func stackConfigBool(ctx context.Context, dir render.Dir, key string, worktree bool) (bool, error) {
	args := []string{"config"}
	if worktree {
		args = append(args, "--worktree")
	}
	args = append(args, "--bool", "--get", key)
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", args)
	if err != nil {
		return false, err
	}
	if code == 1 {
		return false, nil
	}
	if code != 0 {
		return false, fmt.Errorf("stack new: read %s: %s", key, strings.TrimSpace(stderr))
	}
	return strconv.ParseBool(strings.TrimSpace(out))
}

func stackReadSparse(ctx context.Context, dir render.Dir) (stackSparse, error) {
	perWorktree, err := stackConfigBool(ctx, dir, "extensions.worktreeConfig", false)
	if err != nil {
		return stackSparse{}, err
	}
	active, err := stackConfigBool(ctx, dir, "core.sparseCheckout", true)
	if err != nil {
		return stackSparse{}, err
	}
	if !perWorktree || !active {
		return stackSparse{}, errors.New("stack new: --sparse requires active per-worktree sparse configuration in the caller")
	}
	cone, err := stackConfigBool(ctx, dir, "core.sparseCheckoutCone", true)
	if err != nil {
		return stackSparse{}, err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout"})
	if err != nil {
		return stackSparse{}, err
	}
	patterns, err := os.ReadFile(strings.TrimSpace(out))
	if err != nil {
		return stackSparse{}, err
	}
	return stackSparse{patterns: patterns, cone: cone}, nil
}

func stackWriteSparse(ctx context.Context, dir render.Dir, sparse stackSparse) error {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout"})
	if err != nil {
		return err
	}
	path := strings.TrimSpace(out)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, sparse.patterns, 0o644); err != nil {
		return err
	}
	for _, setting := range [][2]string{{"core.sparseCheckout", "true"}, {"core.sparseCheckoutCone", strconv.FormatBool(sparse.cone)}} {
		if _, err := render.RunCLI(ctx, dir, "git", []string{"config", "--worktree", setting[0], setting[1]}); err != nil {
			return err
		}
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"read-tree", "--reset", "-u", "HEAD"}); err != nil {
		return fmt.Errorf("stack new: populate sparse child: %w", err)
	}
	return nil
}

func stackVerifyNewParent(ctx context.Context, dir render.Dir, common string, receipt *stackPublication) error {
	source, err := stackRevParse(ctx, dir, gtRestackRef(receipt.Branch))
	if err != nil {
		return err
	}
	if source != receipt.Source {
		return fmt.Errorf("stack new: %s source changed since publication; publish it before creating a published child", receipt.Branch)
	}
	last, err := gtmeta.LastSubmitted(ctx, common)
	if err != nil {
		return err
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, gtRestackRef(receipt.Branch))
	if err != nil {
		return err
	}
	heads, err := stackRemoteHeads(ctx, dir, remote, []string{receipt.Branch})
	if err != nil {
		return err
	}
	branch := stackRebaseBranch{Name: receipt.Branch, Local: source, Remote: heads[receipt.Branch]}
	if err := stackUsePublication(&branch, receipt, last[receipt.Branch]); err != nil {
		return err
	}
	tx := fmt.Sprintf("start\nverify %s %s\nverify %s %s\ncommit\n", gtRestackRef(receipt.Branch), receipt.Source, stackPublicationRef(receipt.Branch, "receipt"), receipt.OID)
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx)); err != nil {
		return fmt.Errorf("stack new: parent changed during child creation: %w", err)
	}
	return nil
}

func stackTrackPublished(ctx context.Context, dir render.Dir, errW io.Writer, common, name string, receipt *stackPublication) error {
	result, runErr := gtRun(ctx, dir, []string{"track", "--force", "--no-interactive"}, errW)
	if err := gtReport(ctx, errW, result); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("stack new: track published child: %w", runErr)
	}
	if err := gtmeta.Reparent(ctx, common, map[string]string{name: receipt.Branch}); err != nil {
		return err
	}
	return gtmeta.RecordRestacked(ctx, common, map[string]string{name: receipt.Head})
}
