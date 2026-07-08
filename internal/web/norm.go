package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// NormalizeURL canonicalizes raw for use as a cache key: it lowercases the
// scheme and host, drops the default port (80 for http, 443 for https), strips
// tracking query params (utm_*, fbclid, gclid, ref) and byte-sorts the rest, and
// drops the fragment unless it is an SPA hash route (#/ or #!). The path is left
// byte-for-byte.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", raw, err)
	}

	u.Scheme = strings.ToLower(u.Scheme)

	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host

	q := u.Query()
	for k := range q {
		if isTrackingParam(k) {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()

	if !strings.HasPrefix(u.Fragment, "/") && !strings.HasPrefix(u.Fragment, "!") {
		u.Fragment = ""
	}

	return u.String(), nil
}

func isTrackingParam(key string) bool {
	k := strings.ToLower(key)
	return strings.HasPrefix(k, "utm_") || k == "fbclid" || k == "gclid" || k == "ref"
}

// CacheKey is the first 16 hex characters of the SHA-256 of a normalized URL.
func CacheKey(normURL string) string {
	sum := sha256.Sum256([]byte(normURL))
	return hex.EncodeToString(sum[:])[:16]
}
