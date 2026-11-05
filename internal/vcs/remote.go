package vcs

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// GitPushRemote returns the remote git is configured to push branch to, empty
// when neither branch.<branch>.pushRemote nor remote.pushDefault is set.
func GitPushRemote(ctx context.Context, dir render.Dir, branch string) (string, error) {
	for _, key := range []string{"branch." + branch + ".pushRemote", "remote.pushDefault"} {
		out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get", key})
		if err != nil {
			return "", fmt.Errorf("git config %s: %w", key, err)
		}
		switch code {
		case 0:
			if remote := strings.TrimSpace(out); remote != "" {
				return remote, nil
			}
		case 1:
		default:
			return "", fmt.Errorf("git config %s: exit %d: %s", key, code, strings.TrimSpace(stderr))
		}
	}
	return "", nil
}

// RemotesOwnedBy lists the remotes whose URL names a repository of owner.
func RemotesOwnedBy(ctx context.Context, dir render.Dir, owner string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"remote"})
	if err != nil {
		return nil, fmt.Errorf("git remote: %w", err)
	}
	var owned []string
	for remote := range strings.FieldsSeq(out) {
		got, ok, err := RemoteOwner(ctx, dir, remote)
		if err != nil {
			return nil, err
		}
		if ok && strings.EqualFold(got, owner) {
			owned = append(owned, remote)
		}
	}
	return owned, nil
}

// RemoteOwner reads the owner a remote's URL names; ok is false when the URL is
// not host/owner/repo, as with a path on disk.
func RemoteOwner(ctx context.Context, dir render.Dir, remote string) (owner string, ok bool, err error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"remote", "get-url", remote})
	if err != nil {
		return "", false, fmt.Errorf("git remote get-url %s: %w", remote, err)
	}
	owner, ok = urlOwner(strings.TrimSpace(out))
	return owner, ok, nil
}

func urlOwner(raw string) (string, bool) {
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", false
		}
		path = u.Path
	} else {
		host, rest, found := strings.Cut(raw, ":")
		if !found || strings.Contains(host, "/") {
			return "", false
		}
		path = rest
	}
	segments := strings.Split(strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/"), "/")
	if len(segments) != 2 {
		return "", false
	}
	return segments[0], true
}
