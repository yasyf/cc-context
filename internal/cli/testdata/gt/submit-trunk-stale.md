NOT RECORDED. gt submit refusing because trunk is behind (gtTrunkStale
"Aborting submit because trunk branch is out of date"), which classifyGTSubmit
turns into the gt sync advice.

What it needs: the same permitted repo as submit-restack-needed, with the remote
trunk ahead of the local one. Same blocker: reachable only past Graphite's
repository resolution, and a successful path would push.
