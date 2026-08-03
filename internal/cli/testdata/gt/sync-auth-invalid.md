gt sync in a repo with a remote, with no Graphite token.

Recorded with no token but a reachable network — the answer comes from
Graphite's server, so recording this one behind the offline proxy would capture
the connection failure instead. Exit 1. Pins gtSyncAuthRequired2 ("Your Graphite
auth token is invalid/expired"), which classifyGTRestack turns into the gt auth
advice; gt words a missing token that way once a remote exists to ask about.
