package ghremote

import "testing"

func TestRepo(t *testing.T) {
	for _, tt := range []struct {
		url   string
		owner string
		name  string
		ok    bool
	}{
		{url: "https://github.com/maximhq/bifrost.git", owner: "maximhq", name: "bifrost", ok: true},
		{url: "https://github.com/maximhq/bifrost", owner: "maximhq", name: "bifrost", ok: true},
		{url: "https://github.com/Forge-AI/litestream.git/", owner: "Forge-AI", name: "litestream", ok: true},
		{url: "git@github.com:yasyf/bifrost.git", owner: "yasyf", name: "bifrost", ok: true},
		{url: "ssh://git@github.com/yasyf/bifrost.git", owner: "yasyf", name: "bifrost", ok: true},
		{url: "/tmp/remotes/origin.git"},
		{url: "file:///tmp/yasyf/bifrost.git"},
		{url: "https://github.com/bifrost"},
		{url: "https://github.com/a/b/c.git"},
		{url: "git@github.com:/bifrost.git"},
	} {
		t.Run(tt.url, func(t *testing.T) {
			owner, name, ok := Repo(tt.url)
			if owner != tt.owner || name != tt.name || ok != tt.ok {
				t.Errorf("Repo(%q) = %q, %q, %v; want %q, %q, %v", tt.url, owner, name, ok, tt.owner, tt.name, tt.ok)
			}
		})
	}
}
