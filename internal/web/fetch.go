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

	"github.com/yasyf/cc-context/internal/vendor"
)

// cascadeDeadline bounds the whole fetch cascade across every tier, independent
// of the per-tier timeouts.
const cascadeDeadline = 120 * time.Second

// dnsGateTimeout bounds the best-effort split-DNS resolve that runs before the
// hosted tiers; a slow resolver must never stall the cascade.
const dnsGateTimeout = time.Second

// renderDeadline bounds the whole render-escalation chain across every lane,
// independent of each lane's own timeout.
const renderDeadline = 120 * time.Second

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
	// entirely and use only plain HTTP, with no stealth fallthrough. A public
	// hostname that resolves entirely to local addresses (split-horizon DNS) is
	// local too, caught by a best-effort resolve before the hosted tiers run.
	host := u.Hostname()
	if localTarget(host) || (net.ParseIP(host) == nil && t.resolvesLocal(ctx, host)) {
		return t.plainHTTP(ctx, normURL, prior)
	}

	type tierRun struct {
		name Tier
		run  func() (FetchResult, error)
	}
	runs := []tierRun{
		{TierJina, func() (FetchResult, error) { return t.jina(ctx, normURL, false) }},
	}
	if key := os.Getenv(envExaKey); key != "" {
		runs = append(runs, tierRun{TierExa, func() (FetchResult, error) { return t.exa(ctx, normURL, key) }})
	}
	if key := os.Getenv(envFirecrawlKey); key != "" {
		runs = append(runs, tierRun{TierFirecrawl, func() (FetchResult, error) { return t.firecrawl(ctx, normURL, key, false) }})
	}
	runs = append(runs, tierRun{TierHTTP, func() (FetchResult, error) { return t.plainHTTP(ctx, normURL, prior) }})

	var failures []error
	stealth := false
	for _, r := range runs {
		res, err := r.run()
		if t.onAttempt != nil {
			t.onAttempt(r.name, err)
		}
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
			// The joined failures render as text, not %w: they carry
			// errStealthRequired, and that sentinel never escapes fetch into
			// the errors.Is chain.
			return FetchResult{}, fmt.Errorf(
				"%s is unset; cannot fetch a page that requires stealth (earlier failures: %s): %w",
				envBrowserbaseKey, errors.Join(failures...).Error(), ErrBlocked)
		}
		res, err := t.browserbase(ctx, normURL, key)
		if t.onAttempt != nil {
			t.onAttempt(TierBrowserbase, err)
		}
		switch {
		case err == nil:
			return res, nil
		case errors.Is(err, ErrBlocked), errors.Is(err, ErrGone), errors.Is(err, ErrAuthRequired):
			return FetchResult{}, err
		default:
			slog.Warn("web fetch browserbase failed", "url", normURL, "err", err)
			failures = append(failures, err)
		}
	}

	// The joined failures render as text, not %w: they may carry
	// errStealthRequired (when the browserbase attempt itself failed for an
	// ordinary reason), and that sentinel never escapes fetch into the
	// errors.Is chain.
	return FetchResult{}, fmt.Errorf("all fetch tiers failed for %q: %s", normURL, errors.Join(failures...).Error())
}

// RenderFetch escalates a thin cascade result through the render chain — jina
// render, firecrawl render, then the local agent-browser lane — returning the
// first non-thin render, or the largest thin render with stillThin=true when
// every lane still came back thin. The base cascade already produced servable
// content, so a total chain failure returns an error the caller falls back on by
// serving the thin original.
func RenderFetch(ctx context.Context, normURL string) (FetchResult, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, renderDeadline)
	defer cancel()
	return newTiers().renderFetch(ctx, normURL)
}

// renderFetch runs the render lanes in order under ctx. Every lane runs at most
// once and fires t.onAttempt. Any lane error — including errStealthRequired,
// ErrGone, and ErrAuthRequired — is swallowed here and the chain moves on: the
// base cascade already served content, so a leaked sentinel would wrongly abort
// an op whose page IS served. The first non-thin result wins; when every lane is
// thin the largest is returned with stillThin=true; when every lane errors the
// joined failures render as text (never %w), so no sentinel escapes into the
// caller's errors.Is chain.
func (t *tiers) renderFetch(ctx context.Context, normURL string) (FetchResult, bool, error) {
	u, err := url.Parse(normURL)
	if err != nil {
		return FetchResult{}, false, fmt.Errorf("renderFetch: parse url %q: %w", normURL, err)
	}
	host := u.Hostname()
	local := localTarget(host) || (net.ParseIP(host) == nil && t.resolvesLocal(ctx, host))

	type laneRun struct {
		name Tier
		run  func() (FetchResult, error)
	}
	var runs []laneRun
	// The hosted render lanes cannot reach a local target; only agent-browser
	// (which drives a local browser) runs for the localhost-dev-SPA case.
	if !local {
		if os.Getenv(envJinaKey) != "" {
			runs = append(runs, laneRun{TierJinaRender, func() (FetchResult, error) { return t.jina(ctx, normURL, true) }})
		}
		if key := os.Getenv(envFirecrawlKey); key != "" {
			runs = append(runs, laneRun{TierFirecrawlRender, func() (FetchResult, error) { return t.firecrawl(ctx, normURL, key, true) }})
		}
	}
	if vendor.LookPath(agentBrowserBin) != "" {
		runs = append(runs, laneRun{TierAgentBrowser, func() (FetchResult, error) { return t.agentBrowser(ctx, normURL, local) }})
	}
	if len(runs) == 0 {
		return FetchResult{}, false, errors.New("no render lane available (set JINA_API_KEY or FIRECRAWL_API_KEY, or install agent-browser)")
	}

	var (
		failures []error
		best     FetchResult
		haveBest bool
	)
	for _, r := range runs {
		res, err := r.run()
		if t.onAttempt != nil {
			t.onAttempt(r.name, err)
		}
		if err != nil {
			slog.Warn("web render lane failed", "lane", r.name, "url", normURL, "err", err)
			failures = append(failures, fmt.Errorf("%s: %w", r.name, err))
			continue
		}
		if !thinSignature(thinInput{Markdown: res.Markdown, HTML: res.HTML}) {
			return withLinks(res), false, nil
		}
		if !haveBest || len(res.Markdown) > len(best.Markdown) {
			best, haveBest = res, true
		}
	}
	if haveBest {
		return withLinks(best), true, nil
	}
	return FetchResult{}, false, fmt.Errorf("all render lanes failed for %q: %s", normURL, errors.Join(failures...).Error())
}

// withLinks appends the jina render pass's link summary as a ## Links section to
// res.Markdown, after thinness has been classified on the content alone, so a
// nav-heavy page's slugs become chunkable without padding a thin body past the
// floor. It is a no-op when the result carries no links.
func withLinks(res FetchResult) FetchResult {
	if len(res.Links) > 0 {
		res.Markdown += linksSection(res.Links)
	}
	return res
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
		return isLocalIP(ip)
	}
	return false
}

// isLocalIP reports whether ip is a loopback, private, link-local, or
// unspecified address — one only this host can route to.
func isLocalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// resolvesLocal reports whether host — a non-IP-literal name the literal
// localTarget check already passed — resolves entirely to local addresses, the
// split-horizon case where an internal name like wiki.corp.example points at
// 10.0.0.5. It is best-effort: a resolution failure, an empty answer, or any
// public address returns false so the hosted cascade still runs (a hosted
// resolver may legitimately see a different, public address).
func (t *tiers) resolvesLocal(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, dnsGateTimeout)
	defer cancel()
	ips, err := t.lookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !isLocalIP(ip) {
			return false
		}
	}
	return true
}
