The configuration API accepts a small set of typed parameters and returns a validated config object. Every parameter is checked at load time so a missing or malformed value fails loudly at startup rather than lazily on first use, which keeps long-running services predictable and easy to debug in production.

## Parameters

| Name   | Type   | Description                        |
|--------|--------|------------------------------------|
| id     | string | Unique identifier for the record   |
| count  | int    | Number of items to return          |
| strict | bool   | Fail on the first validation error |

## Example

The snippet below loads a config and prints it:

```
func main() {
    cfg, err := Load("config.toml")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(cfg)
}
```

The ~~legacy loader~~ new loader is the supported path going forward.
