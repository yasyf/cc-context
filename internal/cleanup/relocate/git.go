package relocate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func (r *Relocator) command(ctx context.Context, git string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.CommandContext(ctx, git, append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "--no-optional-locks"}, args...)...)
	cmd.Env = r.cfg.GitEnv
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	return cmd, stderr
}

func (r *Relocator) git(ctx context.Context, git string, args ...string) (string, error) {
	cmd, stderr := r.command(ctx, git, args...)
	stdout, err := cmd.Output()
	if err != nil {
		return string(stdout), gitFailure(args, err, stderr)
	}
	return string(stdout), nil
}

func (r *Relocator) stream(ctx context.Context, git string, delim byte, visit func(head []byte), args ...string) error {
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
		stdout.Close()
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

func (r *Relocator) hasGitlink(ctx context.Context, git, tree string) (bool, error) {
	found := false
	err := r.stream(ctx, git, '\n', func(head []byte) {
		if bytes.HasPrefix(head, []byte("160000 ")) {
			found = true
		}
	}, "-C", tree, "ls-files", "--stage")
	return found, err
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
