package web

import "testing"

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"lowercase scheme and host", "HTTP://Example.COM/Path/To", "http://example.com/Path/To", false},
		{"path left byte-for-byte", "http://example.com/A/b/C-D_e", "http://example.com/A/b/C-D_e", false},
		{"strip default port 80", "http://example.com:80/x", "http://example.com/x", false},
		{"strip default port 443", "https://example.com:443/x", "https://example.com/x", false},
		{"keep non-default port", "http://example.com:8080/x", "http://example.com:8080/x", false},
		{"drop all utm params", "http://example.com/?utm_source=foo&utm_medium=bar", "http://example.com/", false},
		{"drop fbclid gclid ref keep rest", "http://example.com/?ref=twitter&gclid=abc&fbclid=xyz&keep=1", "http://example.com/?keep=1", false},
		{"keep page query", "http://example.com/?page=2", "http://example.com/?page=2", false},
		{"sort query bytewise", "http://example.com/?b=2&a=1", "http://example.com/?a=1&b=2", false},
		{"drop sort and keep combined", "http://example.com/?utm_source=x&z=1&a=2&fbclid=z", "http://example.com/?a=2&z=1", false},
		{"strip ordinary fragment", "http://example.com/page#section", "http://example.com/page", false},
		{"keep spa slash fragment", "http://example.com/app#/users/1", "http://example.com/app#/users/1", false},
		{"keep spa bang fragment", "http://example.com/app#!/route", "http://example.com/app#!/route", false},
		{"strip default port on ipv6", "http://[::1]:80/x", "http://[::1]/x", false},
		{"keep non-default port on ipv6", "http://[::1]:8080/x", "http://[::1]:8080/x", false},
		{"everything at once", "HTTPS://Example.com:443/Docs/?utm_campaign=x&z=1&a=2#/spa", "https://example.com/Docs/?a=2&z=1#/spa", false},
		{"control character rejected", "http://exam\x7fple.com/", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeURL(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCacheKey(t *testing.T) {
	const in = "https://go.dev/"
	const want = "ba6e07bbb6027efa"

	got := CacheKey(in)
	if got != want {
		t.Errorf("CacheKey(%q) = %q, want %q", in, got, want)
	}
	if len(got) != 16 {
		t.Errorf("CacheKey(%q) len = %d, want 16", in, len(got))
	}
	if other := CacheKey("https://go.dev/doc/"); other == got {
		t.Errorf("CacheKey collided for distinct inputs: %q", got)
	}
}
