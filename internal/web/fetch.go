package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// cascadeDeadline bounds the whole fetch cascade across every tier, independent
// of the per-tier timeouts.
const cascadeDeadline = 120 * time.Second

// ErrNotModified reports that a conditional revalidation (plain-HTTP tier, prior
// non-nil) returned 304: the caller's cached page is still current and its
// chunks and vectors should be kept. It is a fetch outcome, not a failure, so
// callers branch on it with errors.Is before treating an error as a real fault.
var ErrNotModified = errors.New("not modified since prior fetch")

// Fetch retrieves normURL through the tier cascade — jina, then the keyed exa
// and firecrawl tiers, then plain HTTP, with browserbase as the stealth backstop
// — and returns the first tier's clean result. prior, when non-nil, drives
// plain-HTTP conditional revalidation.
//
// A target 404/410 aborts with ErrGone and a target 401 with ErrAuthRequired,
// from whichever tier observes it; a 304 revalidation returns ErrNotModified. A
// page that trips a bot/DDoS challenge routes to browserbase, and when no
// BROWSERBASE_API_KEY is set the cascade fails with ErrBlocked naming the var. A
// challenge body is never returned as success, so it never reaches the store.
// When every tier fails for ordinary reasons the errors are joined.
func Fetch(ctx context.Context, normURL string, prior *Page) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cascadeDeadline)
	defer cancel()
	return newTiers().fetch(ctx, normURL, prior)
}

func (t *tiers) fetch(ctx context.Context, normURL string, prior *Page) (FetchResult, error) {
	u, err := url.Parse(normURL)
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch: parse url %q: %w", normURL, err)
	}
	// A local target is unreachable by any hosted reader; keep its URL off them
	// entirely and use only plain HTTP, with no stealth fallthrough.
	if localTarget(u.Hostname()) {
		return t.plainHTTP(ctx, normURL, prior)
	}

	type tierRun struct {
		name Tier
		run  func() (FetchResult, error)
	}
	runs := []tierRun{
		{TierJina, func() (FetchResult, error) { return t.jina(ctx, normURL) }},
	}
	if key := os.Getenv(envExaKey); key != "" {
		runs = append(runs, tierRun{TierExa, func() (FetchResult, error) { return t.exa(ctx, normURL, key) }})
	}
	if key := os.Getenv(envFirecrawlKey); key != "" {
		runs = append(runs, tierRun{TierFirecrawl, func() (FetchResult, error) { return t.firecrawl(ctx, normURL, key) }})
	}
	runs = append(runs, tierRun{TierHTTP, func() (FetchResult, error) { return t.plainHTTP(ctx, normURL, prior) }})

	var failures []error
	stealth := false
	for _, r := range runs {
		res, err := r.run()
		switch {
		case err == nil:
			return res, nil
		case errors.Is(err, ErrGone), errors.Is(err, ErrAuthRequired), errors.Is(err, ErrNotModified):
			return FetchResult{}, err
		case errors.Is(err, errStealthRequired):
			slog.Warn("web fetch tier needs stealth", "tier", r.name, "url", normURL, "err", err)
			stealth = true
			failures = append(failures, err)
		default:
			// Keyless jina rejects with a service 401 — the expected path for
			// users without JINA_API_KEY, not a warning-worthy failure.
			if r.name == TierJina && os.Getenv(envJinaKey) == "" {
				slog.Debug("web fetch tier failed", "tier", r.name, "url", normURL, "err", err)
			} else {
				slog.Warn("web fetch tier failed", "tier", r.name, "url", normURL, "err", err)
			}
			failures = append(failures, err)
		}
	}

	if stealth {
		key := os.Getenv(envBrowserbaseKey)
		if key == "" {
			return FetchResult{}, fmt.Errorf("%s is unset; cannot fetch a page that requires stealth: %w", envBrowserbaseKey, ErrBlocked)
		}
		res, err := t.browserbase(ctx, normURL, key)
		switch {
		case err == nil:
			return res, nil
		case errors.Is(err, ErrBlocked):
			return FetchResult{}, err
		default:
			slog.Warn("web fetch browserbase failed", "url", normURL, "err", err)
			failures = append(failures, err)
		}
	}

	return FetchResult{}, fmt.Errorf("all fetch tiers failed for %q: %w", normURL, errors.Join(failures...))
}

// localTarget reports whether host addresses a machine only this host can reach —
// a loopback, private, link-local, or unspecified IP, or the localhost, *.local,
// or *.internal names. Hosted reader tiers cannot fetch such a target, so the
// cascade must never hand them its URL.
func localTarget(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}
