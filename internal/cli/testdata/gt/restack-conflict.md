gt restack over a branch whose commit conflicts with trunk's.

Recorded offline, no token. gt writes the conflict banner to stdout and exits 1.
Pins gtSyncConflict ("Hit conflict restacking"), the sentence classifyGTRestack
turns into the gt continue / gt abort advice. gt sync prints the same banner
when its restack phase conflicts, and sync cannot run offline, so restack is
where the wording is recorded.
