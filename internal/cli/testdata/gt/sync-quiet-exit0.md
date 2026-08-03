The everyday sync: nothing to do, tips off, stderr empty.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 0 with both streams saying only that
there was nothing to do. It is the control for sync-tips-exit0 — same repo
state, same argv, tips the only difference — which is what isolates the 573
bytes that scenario puts on stderr as tips rather than as anything gt had to
report.
