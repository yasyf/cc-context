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
