package vendor

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestAssetFor(t *testing.T) {
	tests := []struct {
		name    string
		p       platform
		want    asset
		wantErr bool
	}{
		{"darwin/arm64", platform{"darwin", "arm64"}, asset{"tilth-aarch64-apple-darwin.tar.gz", false}, false},
		{"darwin/amd64", platform{"darwin", "amd64"}, asset{"tilth-x86_64-apple-darwin.tar.gz", false}, false},
		{"linux/arm64", platform{"linux", "arm64"}, asset{"tilth-aarch64-unknown-linux-musl.tar.gz", false}, false},
		{"linux/amd64", platform{"linux", "amd64"}, asset{"tilth-x86_64-unknown-linux-musl.tar.gz", false}, false},
		{"windows/amd64", platform{"windows", "amd64"}, asset{"tilth-x86_64-pc-windows-msvc.zip", true}, false},
		{"unsupported windows/arm64", platform{"windows", "arm64"}, asset{}, true},
		{"unsupported linux/386", platform{"linux", "386"}, asset{}, true},
		{"unsupported freebsd/amd64", platform{"freebsd", "amd64"}, asset{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := assetFor(tt.p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assetFor(%v) err = %v, wantErr %v", tt.p, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("assetFor(%v) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}

func TestBinaryName(t *testing.T) {
	tests := []struct {
		name string
		a    asset
		want string
	}{
		{"unix", asset{"tilth-aarch64-apple-darwin.tar.gz", false}, "tilth-v0.9.0"},
		{"windows", asset{"tilth-x86_64-pc-windows-msvc.zip", true}, "tilth-v0.9.0.exe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := binaryName(tt.a); got != tt.want {
				t.Errorf("binaryName(%v) = %q, want %q", tt.a, got, tt.want)
			}
		})
	}
}

func TestAssetURL(t *testing.T) {
	const name = "tilth-aarch64-apple-darwin.tar.gz"
	want := "https://github.com/jahala/tilth/releases/download/v0.9.0/" + name
	if got := assetURL(name); got != want {
		t.Errorf("assetURL(%q) = %q, want %q", name, got, want)
	}
}

func TestVerify(t *testing.T) {
	// A pinned asset whose archive matches its committed digest verifies; any
	// other asset bytes (tamper) or unpinned name is a hard error.
	const name = "tilth-aarch64-apple-darwin.tar.gz"
	want := assetChecksums[name]

	archive := []byte("the exact bytes of the pinned asset")
	sum := sha256.Sum256(archive)
	t.Cleanup(func() { assetChecksums[name] = want })
	assetChecksums[name] = hex.EncodeToString(sum[:])

	if err := verify(name, archive); err != nil {
		t.Fatalf("verify matching archive: %v", err)
	}
	if err := verify(name, []byte("tampered")); err == nil {
		t.Fatal("verify tampered archive: want error, got nil")
	}
	if err := verify("not-a-pinned-asset.tar.gz", archive); err == nil {
		t.Fatal("verify unpinned asset: want error, got nil")
	}
}
