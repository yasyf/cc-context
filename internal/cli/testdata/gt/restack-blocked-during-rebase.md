gt restack run a second time, with the conflicted rebase from the first still
open.

Recorded offline, no token. Exit 1 with an ERROR:-led diagnostic on stderr and
nothing on stdout. Pins an unrecognized failure — classifyGTRestack wraps it
verbatim — and the ERROR: prefix Diagnostics and reportedError read.
