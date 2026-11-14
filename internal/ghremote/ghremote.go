// Package ghremote reads the GitHub repository a git remote's URL names.
package ghremote

import (
	"net/url"
	"strings"
)

// Repo returns the owner and name of the repository raw names as
// host/owner/name, in any of git's URL forms; ok is false for a URL that names
// none, such as a path on disk.
func Repo(raw string) (owner, name string, ok bool) {
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", "", false
		}
		path = u.Path
	} else {
		host, rest, found := strings.Cut(raw, ":")
		if !found || strings.Contains(host, "/") {
			return "", "", false
		}
		path = rest
	}
	owner, name, found := strings.Cut(strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/"), "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}
