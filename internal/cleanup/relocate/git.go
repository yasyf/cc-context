package relocate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/render"
)

const readTimeout = time.Minute

func (r *Relocator) command(ctx context.Context, git string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.CommandContext(ctx, git, append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "--no-optional-locks"}, args...)...) //nolint:gosec // git is the absolute path the request validated; the argv is the relocator's own
	cmd.Env = r.cfg.GitEnv
	render.BoundProbe(cmd)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	return cmd, stderr
}

func (r *Relocator) git(ctx context.Context, git string, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	out, err := r.run(bounded, git, args...)
	return out, unprobed(ctx, bounded, err)
}

func (r *Relocator) rewrite(ctx context.Context, git string, args ...string) (string, error) {
	return r.run(context.WithoutCancel(ctx), git, args...)
}

func (r *Relocator) run(ctx context.Context, git string, args ...string) (string, error) {
	cmd, stderr := r.command(ctx, git, args...)
	stdout, err := cmd.Output()
	if err != nil {
		return string(stdout), gitFailure(args, err, stderr)
	}
	return string(stdout), nil
}

func unprobed(ctx, bounded context.Context, err error) error {
	if err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", cleanup.ErrUnprobed, err)
	}
	return err
}

func gitReason(err error) string {
	if errors.Is(err, cleanup.ErrUnprobed) {
		return "timeout"
	}
	return "git"
}

func (r *Relocator) stream(ctx context.Context, git string, delim byte, visit func(head []byte), args ...string) error {
	bounded, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return unprobed(ctx, bounded, r.scan(bounded, git, delim, visit, args...))
}

func (r *Relocator) scan(ctx context.Context, git string, delim byte, visit func(head []byte), args ...string) error {
	cmd, stderr := r.command(ctx, git, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	scanErr := eachRecord(stdout, delim, visit)
	if scanErr != nil {
		scanErr = errors.Join(scanErr, stdout.Close())
	}
	if err := cmd.Wait(); err != nil {
		return gitFailure(args, err, stderr)
	}
	if scanErr != nil {
		return fmt.Errorf("git %s: read output: %w", strings.Join(args, " "), scanErr)
	}
	return nil
}

func gitFailure(args []string, err error, stderr *bytes.Buffer) error {
	said := strings.TrimSpace(stderr.String())
	if said == "" {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, said)
}

func quietMiss(err error, stdout string) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1 && stdout == ""
}

func eachRecord(r io.Reader, delim byte, visit func(head []byte)) error {
	reader := bufio.NewReader(r)
	for {
		head, err := reader.ReadSlice(delim)
		if len(head) > 0 {
			visit(bytes.TrimSuffix(head, []byte{delim}))
		}
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = reader.ReadSlice(delim)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (r *Relocator) head(ctx context.Context, git, tree string) (string, error) {
	out, err := r.git(ctx, git, "-C", tree, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if quietMiss(err, out) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (r *Relocator) branch(ctx context.Context, git, tree string) (string, error) {
	out, err := r.git(ctx, git, "-C", tree, "symbolic-ref", "--quiet", "--short", "HEAD")
	if quietMiss(err, out) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (r *Relocator) checkedOutSubmodule(ctx context.Context, git, tree string) (string, error) {
	var found string
	var inspect error
	err := r.stream(ctx, git, 0, func(head []byte) {
		meta, name, ok := bytes.Cut(head, []byte{'\t'})
		if found != "" || inspect != nil || !ok || !bytes.HasPrefix(meta, []byte("160000 ")) {
			return
		}
		path := filepath.Join(tree, string(name))
		_, err := os.Lstat(filepath.Join(path, ".git"))
		switch {
		case err == nil:
			found = path
		case !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
			inspect = err
		}
	}, "-C", tree, "ls-files", "--stage", "-z")
	return found, errors.Join(err, inspect)
}

func (r *Relocator) pushed(ctx context.Context, git, repo, branch, head string) (bool, error) {
	if head == "" {
		return true, nil
	}
	if branch != "" {
		out, err := r.git(ctx, git, "--git-dir="+repo, "config", "--get", "branch."+branch+".remote")
		if err != nil && !quietMiss(err, out) {
			return false, err
		}
		remote := strings.TrimSpace(out)
		if remote == "" || remote == "." {
			remote = "origin"
		}
		ref := "refs/remotes/" + remote + "/" + branch
		out, err = r.git(ctx, git, "--git-dir="+repo, "show-ref", "--verify", "--quiet", ref)
		if err == nil {
			out, err = r.git(ctx, git, "--git-dir="+repo, "merge-base", "--is-ancestor", head, ref)
			if err == nil {
				return true, nil
			}
		}
		if !quietMiss(err, out) {
			return false, err
		}
	}
	out, err := r.git(ctx, git, "--git-dir="+repo, "for-each-ref", "--count=1", "--contains", head, "--format=%(refname)", "refs/remotes/")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

const dirtyShown = 5

func (r *Relocator) dirt(ctx context.Context, git, tree string) (string, error) {
	var (
		shown    []string
		total    int
		original bool
	)
	err := r.stream(ctx, git, 0, func(head []byte) {
		if original {
			original = false
			return
		}
		status, name := head, head
		if len(head) > 3 {
			status, name = head[:2], head[3:]
		}
		original = bytes.ContainsAny(status, "RC")
		total++
		if len(shown) < dirtyShown {
			shown = append(shown, string(name))
		}
	}, "-C", tree, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil || total == 0 {
		return "", err
	}
	detail := "uncommitted changes: " + strings.Join(shown, ", ")
	if total > len(shown) {
		detail += fmt.Sprintf(" and %d more", total-len(shown))
	}
	return detail, nil
}
