gt sync in a repo with no git remote.

Recorded offline, no token. Exit 1: gt resolves the repo through its own API
before touching git, so with no remote to name the repo it never gets that far.
Pins an unrecognized sync failure — classifyGTRestack wraps it verbatim — plus
the tip block gt writes to stderr ahead of its ERROR: line.
