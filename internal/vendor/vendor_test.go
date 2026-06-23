package vendor

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestToolAssetFor(t *testing.T) {
	tests := []struct {
		name    string
		tool    Tool
		p       platform
		want    string
		wantErr bool
	}{
		{"tilth darwin/arm64", Tilth, platform{"darwin", "arm64"}, "tilth-aarch64-apple-darwin.tar.gz", false},
		{"tilth darwin/amd64", Tilth, platform{"darwin", "amd64"}, "tilth-x86_64-apple-darwin.tar.gz", false},
		{"tilth linux/arm64", Tilth, platform{"linux", "arm64"}, "tilth-aarch64-unknown-linux-musl.tar.gz", false},
		{"tilth linux/amd64", Tilth, platform{"linux", "amd64"}, "tilth-x86_64-unknown-linux-musl.tar.gz", false},
		{"tilth windows/amd64", Tilth, platform{"windows", "amd64"}, "tilth-x86_64-pc-windows-msvc.zip", false},
		{"tilth unsupported freebsd", Tilth, platform{"freebsd", "amd64"}, "", true},
		{"ast-grep darwin/arm64", AstGrep, platform{"darwin", "arm64"}, "app-aarch64-apple-darwin.zip", false},
		{"ast-grep darwin/amd64", AstGrep, platform{"darwin", "amd64"}, "app-x86_64-apple-darwin.zip", false},
		{"ast-grep linux/arm64", AstGrep, platform{"linux", "arm64"}, "app-aarch64-unknown-linux-gnu.zip", false},
		{"ast-grep linux/amd64", AstGrep, platform{"linux", "amd64"}, "app-x86_64-unknown-linux-gnu.zip", false},
		{"ast-grep unsupported windows", AstGrep, platform{"windows", "amd64"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.tool.assetFor(tt.p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assetFor(%v) err = %v, wantErr %v", tt.p, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("assetFor(%v) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}

func TestToolAssetURL(t *testing.T) {
	const asset = "app-aarch64-apple-darwin.zip"
	want := "https://github.com/ast-grep/ast-grep/releases/download/0.44.0/" + asset
	if got := AstGrep.assetURL(asset); got != want {
		t.Errorf("assetURL(%q) = %q, want %q", asset, got, want)
	}
}

func TestToolVerify(t *testing.T) {
	// A pinned asset whose archive matches its committed digest verifies; tampered
	// bytes or an unpinned name is a hard error.
	const asset = "app-aarch64-apple-darwin.zip"
	want := AstGrep.Checksums[asset]

	archive := []byte("the exact bytes of the pinned asset")
	sum := sha256.Sum256(archive)
	t.Cleanup(func() { AstGrep.Checksums[asset] = want })
	AstGrep.Checksums[asset] = hex.EncodeToString(sum[:])

	if err := AstGrep.verify(asset, archive); err != nil {
		t.Fatalf("verify matching archive: %v", err)
	}
	if err := AstGrep.verify(asset, []byte("tampered")); err == nil {
		t.Fatal("verify tampered archive: want error, got nil")
	}
	if err := AstGrep.verify("not-a-pinned-asset.zip", archive); err == nil {
		t.Fatal("verify unpinned asset: want error, got nil")
	}
}

// zipWith builds an in-memory zip whose entries are name→content in order.
func zipWith(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e[0])
		if err != nil {
			t.Fatalf("create zip entry %q: %v", e[0], err)
		}
		if _, err := w.Write([]byte(e[1])); err != nil {
			t.Fatalf("write zip entry %q: %v", e[0], err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractZipSelectsByName(t *testing.T) {
	// The ast-grep zip ships "sg" first, then "ast-grep"; the by-name selector
	// must skip "sg" and extract "ast-grep", not the first entry.
	archive := zipWith(t, [][2]string{
		{"sg", "i am sg, the wrong binary"},
		{"ast-grep", "i am ast-grep, the right binary"},
	})
	got, err := AstGrep.extractZip(archive)
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if want := "i am ast-grep, the right binary"; string(got) != want {
		t.Errorf("extractZip selected %q, want %q", got, want)
	}
}

func TestExtractZipMissingEntry(t *testing.T) {
	archive := zipWith(t, [][2]string{{"sg", "only sg here"}})
	if _, err := AstGrep.extractZip(archive); err == nil {
		t.Fatal("extractZip: want error for missing entry, got nil")
	}
}

func TestToolBinaryName(t *testing.T) {
	tests := []struct {
		name  string
		tool  Tool
		asset string
		want  string
	}{
		{"tilth tar.gz", Tilth, "tilth-aarch64-apple-darwin.tar.gz", "tilth-v0.9.0"},
		{"ast-grep zip non-windows", AstGrep, "app-aarch64-apple-darwin.zip", "ast-grep-0.44.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tool.binaryName(tt.asset); got != tt.want {
				t.Errorf("binaryName(%q) = %q, want %q", tt.asset, got, tt.want)
			}
		})
	}
}

func TestResolveOrder(t *testing.T) {
	orig := LookPath
	t.Cleanup(func() { LookPath = orig })

	// Configured bin wins outright, no PATH lookup, no download.
	LookPath = func(string) string {
		t.Fatal("Resolve consulted PATH despite a configured bin")
		return ""
	}
	got, err := Resolve(context.Background(), AstGrep, "/configured/ast-grep")
	if err != nil {
		t.Fatalf("Resolve(configured): %v", err)
	}
	if got != "/configured/ast-grep" {
		t.Errorf("Resolve(configured) = %q, want /configured/ast-grep", got)
	}

	// No configured bin → PATH hit short-circuits the download. A reached Ensure
	// would try the network; we assert the PATH result and the lookup name.
	var lookedUp string
	LookPath = func(name string) string {
		lookedUp = name
		return "/usr/local/bin/ast-grep"
	}
	got, err = Resolve(context.Background(), AstGrep, "")
	if err != nil {
		t.Fatalf("Resolve(PATH): %v", err)
	}
	if got != "/usr/local/bin/ast-grep" {
		t.Errorf("Resolve(PATH) = %q, want /usr/local/bin/ast-grep", got)
	}
	if lookedUp != "ast-grep" {
		t.Errorf("Resolve looked up %q, want ast-grep (never sg)", lookedUp)
	}
}

func TestResolveFallsThroughToEnsure(t *testing.T) {
	// Neither a configured bin nor a PATH hit → Resolve reaches Ensure. Point the
	// download at an unsupported platform so Ensure errors before any network I/O,
	// proving the fall-through without hitting the network.
	orig := LookPath
	t.Cleanup(func() { LookPath = orig })
	LookPath = func(string) string { return "" }

	unsupported := Tool{Name: "ast-grep", Version: "0.44.0", Assets: map[platform]string{}}
	_, err := Resolve(context.Background(), unsupported, "")
	if err == nil {
		t.Fatal("Resolve: want error from Ensure on unsupported platform, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported platform") {
		t.Errorf("Resolve error = %v, want unsupported-platform error from Ensure", err)
	}
}
