package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestPlainHTTPTenMiBCap(t *testing.T) {
	isolateKeys(t)
	// Serve just over the 10 MiB cap; the tier must truncate at the limit.
	big := bytes.Repeat([]byte("a"), maxBodyBytes+512)
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	})
	ts := &tiers{client: &http.Client{}}

	got, err := ts.plainHTTP(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("plainHTTP: %v", err)
	}
	if len(got.HTML) != maxBodyBytes {
		t.Errorf("body len = %d, want the 10 MiB cap %d", len(got.HTML), maxBodyBytes)
	}
}

func TestPlainHTTPUserAgent(t *testing.T) {
	isolateKeys(t)
	var gotUA string
	target := startTarget(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.UserAgent()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html></html>"))
	})
	ts := &tiers{client: &http.Client{}}

	if _, err := ts.plainHTTP(context.Background(), target, nil); err != nil {
		t.Fatalf("plainHTTP: %v", err)
	}
	if !strings.HasPrefix(gotUA, "ccx-web/") {
		t.Errorf("User-Agent = %q, want a ccx-web/ prefix", gotUA)
	}
}

func TestPlainHTTPConditionalHeaders(t *testing.T) {
	isolateKeys(t)
	prior := &Page{ETag: `"abc"`, LastMod: "Mon, 07 Jul 2026 12:00:00 GMT"}
	var ifNoneMatch, ifModSince string
	target := startTarget(t, func(w http.ResponseWriter, r *http.Request) {
		ifNoneMatch = r.Header.Get("If-None-Match")
		ifModSince = r.Header.Get("If-Modified-Since")
		w.WriteHeader(http.StatusNotModified)
	})
	ts := &tiers{client: &http.Client{}}

	_, err := ts.plainHTTP(context.Background(), target, prior)
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("err = %v, want ErrNotModified", err)
	}
	if ifNoneMatch != `"abc"` {
		t.Errorf("If-None-Match = %q, want %q", ifNoneMatch, `"abc"`)
	}
	if ifModSince != prior.LastMod {
		t.Errorf("If-Modified-Since = %q, want %q", ifModSince, prior.LastMod)
	}
}

func TestPlainHTTPCarriesValidators(t *testing.T) {
	isolateKeys(t)
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"srv-etag"`)
		w.Header().Set("Last-Modified", "Tue, 08 Jul 2026 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>hi</body></html>"))
	})
	ts := &tiers{client: &http.Client{}}

	got, err := ts.plainHTTP(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("plainHTTP: %v", err)
	}
	if got.ETag != `"srv-etag"` {
		t.Errorf("ETag = %q, want the response validator", got.ETag)
	}
	if got.LastMod != "Tue, 08 Jul 2026 00:00:00 GMT" {
		t.Errorf("LastMod = %q, want the response validator", got.LastMod)
	}
	if got.HTML != "<html><body>hi</body></html>" {
		t.Errorf("HTML = %q", got.HTML)
	}
}

func TestClassifyTargetStatus(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error // sentinel it must wrap, or nil
		plain   bool  // true when it must be a non-sentinel cascade error
	}{
		{"200 ok", 200, nil, false},
		{"302 followed", 302, nil, false},
		{"404 gone", 404, ErrGone, false},
		{"410 gone", 410, ErrGone, false},
		{"401 auth", 401, ErrAuthRequired, false},
		{"403 stealth", 403, errStealthRequired, false},
		{"429 stealth", 429, errStealthRequired, false},
		{"503 stealth", 503, errStealthRequired, false},
		{"500 cascade", 500, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyTargetStatus(TierHTTP, tt.status)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want it to wrap %v", err, tt.wantErr)
				}
			case tt.plain:
				if err == nil {
					t.Fatalf("err = nil, want a cascade-able failure")
				}
				for _, s := range []error{ErrGone, ErrAuthRequired, errStealthRequired} {
					if errors.Is(err, s) {
						t.Errorf("err = %v, want a plain failure, not %v", err, s)
					}
				}
			default:
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
			}
		})
	}
}

func TestStatusFromText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"embedded 404", "Target URL returned error 404: Not Found", 404},
		{"embedded 503", "503 Service Unavailable", 503},
		{"no status", "the page could not be loaded", 0},
		{"success not matched", "returned 200 OK", 0},
		{"3xx not matched", "redirected 302 times", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusFromText(tt.in); got != tt.want {
				t.Errorf("statusFromText(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestPlainHTTPPxSensorNotChallenge proves the end-to-end raw-path fix: a real
// 200 whose HTML embeds a PerimeterX sensor (window._pxAppId) is returned as
// content, not rejected as a stealth challenge.
func TestPlainHTTPPxSensorNotChallenge(t *testing.T) {
	isolateKeys(t)
	const html = `<html><head><script>window._pxAppId="PXabc";</script></head><body>real product page</body></html>`
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, html)
	})
	ts := &tiers{client: &http.Client{}}

	got, err := ts.plainHTTP(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("plainHTTP: %v (a px sensor on a normal page must not trip the challenge gate)", err)
	}
	if got.HTML != html {
		t.Errorf("HTML = %q, want the page body", got.HTML)
	}
}

func TestPlainHTTPRefusesRedirectToLocal(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{})
	ts.client.CheckRedirect = ts.refuseLocalRedirect
	// A public-mapped target 302s to a loopback address; the gate must refuse the
	// hop before it is fetched and cached under the public URL.
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:11434/internal", http.StatusFound)
	})

	got, err := ts.plainHTTP(context.Background(), target, nil)
	if err == nil {
		t.Fatalf("plainHTTP = %+v, want an error refusing the redirect to a local target", got)
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v, want it to describe a refused redirect", err)
	}
	if got.HTML != "" {
		t.Errorf("HTML = %q, want nothing returned on a refused redirect", got.HTML)
	}
}

// TestPlainHTTPRefusesRedirectToSplitDNSLocal pins the redirect hop to the same
// split-DNS gate as the cascade entry: a public target 302s to a public-looking
// hostname that resolves entirely to a private address, and the hop is refused
// before the internal content can be fetched and cached under the public key.
func TestPlainHTTPRefusesRedirectToSplitDNSLocal(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{})
	ts.lookupIP = func(ctx context.Context, _, host string) ([]net.IP, error) {
		if host == "intra.example" {
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		}
		return publicLookupIP(ctx, "ip", host)
	}
	ts.client.CheckRedirect = ts.refuseLocalRedirect
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://intra.example/secret", http.StatusFound)
	})

	got, err := ts.plainHTTP(context.Background(), target, nil)
	if err == nil {
		t.Fatalf("plainHTTP = %+v, want an error refusing the split-DNS redirect", got)
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v, want it to describe a refused redirect", err)
	}
	if got.HTML != "" {
		t.Errorf("HTML = %q, want nothing returned on a refused redirect", got.HTML)
	}
}
