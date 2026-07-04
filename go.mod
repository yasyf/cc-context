module github.com/yasyf/cc-context

go 1.26.4

require (
	// gomonty embeds the monty sandbox runtime (pins pydantic/monty by git rev); bump only in
	// dedicated PRs that re-run the codeexec suite and the episode replays — never via bulk go get -u.
	github.com/ewhauser/gomonty v0.0.14
	github.com/modelcontextprotocol/go-sdk v1.6.1
	github.com/spf13/cobra v1.10.2
	github.com/toon-format/toon-go v0.0.0-20251202084852-7ca0e27c4e8c
	golang.org/x/sys v0.42.0
)

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
)
