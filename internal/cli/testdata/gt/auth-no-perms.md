The lane's reachability probe against a GitHub repo Graphite is not permitted to
submit to (this one).

Recorded live (CCX_GT_RECORD_TOKEN); reads Graphite's API, writes nothing. Exit
1 with gt's identity line on stdout and the refusal on stderr — the one recorded
scenario whose two streams both carry payload, so it is also gtJoinStreams'
golden. Pins gtProbeNoPerms, whose whole line classifyGTProbe quotes as the
lane's decline note, because that line names the repo.
