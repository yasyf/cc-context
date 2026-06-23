package vendor

// Tilth is the pinned tilth engine binary. The pinned release publishes no
// checksum manifest, so the digests below are the committed integrity control;
// bump them alongside Version.
var Tilth = Tool{
	Name:         "tilth",
	Version:      "v0.9.0",
	ReleaseBase:  "https://github.com/jahala/tilth/releases/download/v0.9.0",
	BinInArchive: "tilth.exe",
	Assets: map[platform]string{
		{"darwin", "arm64"}:  "tilth-aarch64-apple-darwin.tar.gz",
		{"darwin", "amd64"}:  "tilth-x86_64-apple-darwin.tar.gz",
		{"linux", "arm64"}:   "tilth-aarch64-unknown-linux-musl.tar.gz",
		{"linux", "amd64"}:   "tilth-x86_64-unknown-linux-musl.tar.gz",
		{"windows", "amd64"}: "tilth-x86_64-pc-windows-msvc.zip",
	},
	Checksums: map[string]string{
		"tilth-aarch64-apple-darwin.tar.gz":       "cdded363183c8b6ad276c8d049bc3b8b2dfa8c7e57d846c9bb4352f3515595fd",
		"tilth-x86_64-apple-darwin.tar.gz":        "635330817ac68cb3b7192f56f1cdbde27152331afb6369a4ade2f5349b167b2c",
		"tilth-aarch64-unknown-linux-musl.tar.gz": "2fa3ca73f089bdf037c7d5bbb951b5f8d7aa53de834753645a01b12a67cf67b6",
		"tilth-x86_64-unknown-linux-musl.tar.gz":  "6073bc83d3836913195be01bd953c6e0e6058d5774b216b84da71b87d6bf769c",
		"tilth-x86_64-pc-windows-msvc.zip":        "eb277008adf8a50dad3d374d028be6ff9472d9ef0ebc5118d1eb28bfa1c5be7d",
	},
}

// AstGrep is the pinned ast-grep engine binary. Each release zip ships both
// "ast-grep" and "sg"; BinInArchive selects the former. Upstream publishes no
// checksum manifest, so the digests below are self-computed and committed; bump
// them alongside Version (see the bump checklist below).
//
// Bump checklist for Version:
//  1. For each of the four asset URLs under ReleaseBase, `curl -fsSL <url> | shasum -a 256`.
//  2. Replace Version, ReleaseBase, and the four Checksums digests.
//  3. Confirm the zip still ships an "ast-grep" entry (`unzip -l`); update BinInArchive if upstream renames it.
var AstGrep = Tool{
	Name:         "ast-grep",
	Version:      "0.44.0",
	ReleaseBase:  "https://github.com/ast-grep/ast-grep/releases/download/0.44.0",
	BinInArchive: "ast-grep",
	Assets: map[platform]string{
		{"darwin", "arm64"}: "app-aarch64-apple-darwin.zip",
		{"darwin", "amd64"}: "app-x86_64-apple-darwin.zip",
		{"linux", "arm64"}:  "app-aarch64-unknown-linux-gnu.zip",
		{"linux", "amd64"}:  "app-x86_64-unknown-linux-gnu.zip",
	},
	Checksums: map[string]string{
		"app-aarch64-apple-darwin.zip":      "80ad83ae28c56cbbaa2beaa391f564b073a99c2a0a20d49fd9ddc10aaafd6979",
		"app-x86_64-apple-darwin.zip":       "0df15196bd07a598dbc600feb95b5e707c062542be282d3f6ebd92436ef7777e",
		"app-aarch64-unknown-linux-gnu.zip": "86f4d5924b59fca4bbcb3fb2fb9a73b38a4f666c402886395c8bf18b6afc61f0",
		"app-x86_64-unknown-linux-gnu.zip":  "a074982c59a749371d225e6129faf5815f731f460aa080c004af9b7e79c55632",
	},
}
