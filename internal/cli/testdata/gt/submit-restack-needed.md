NOT RECORDED. gt submit refusing a stack that drifted (gtRestackNeeded1 "You
must restack before submitting this stack." / gtRestackNeeded2 "You must restack
and resolve conflicts with "), which classifyGTSubmit turns into the gt restack
advice.

What it needs: a token plus a checkout of a GitHub repo Graphite is permitted to
submit to, holding a tracked branch whose parent has moved. Measured 2026-08-02
at gt 1.8.6: gt resolves the repository through Graphite's API before it looks
at the stack, so against any repo reachable here the run fails at resolution
instead (see submit-repo-unverified). Recording it against a permitted repo
would submit a real pull request, which is why it is not done from a recorder.
