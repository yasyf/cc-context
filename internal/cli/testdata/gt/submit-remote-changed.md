NOT RECORDED. gt submit refusing because the remote branch moved
(gtRemoteChanged1 "This branch has been updated remotely since you last
submitted" / gtRemoteChanged2 "Force-with-lease push failed due to external
changes to the remote branch"), which classifyGTSubmit turns into the reconcile
advice.

What it needs: a permitted repo with a branch already submitted once and since
changed on the remote. Same blocker as submit-restack-needed, and reproducing it
means two real submits.
