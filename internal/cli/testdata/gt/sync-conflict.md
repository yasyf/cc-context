NOT RECORDED. gt sync hitting a conflict while restacking after it pulls trunk.

What it needs: a token and a permitted repo whose trunk moved under a stack that
conflicts with it. gt sync cannot get past repository resolution offline, so the
banner is recorded from gt restack instead — see restack-conflict, which carries
the same gtSyncConflict sentence and is what classifyGTRestack matches.
