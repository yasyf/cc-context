gt sync whose trunk fast-forwards and whose branch then will not restack onto
it, tips off.

Recorded live (CCX_GT_RECORD_TOKEN). **Exit 0**, and the shape gtZeroSurfaces
and restack.go's zero-surfaces path exist for: stdout's restack section is
empty, so the only account of what happened is on stderr. This is the trigger
case for gtResult.Diagnostics — a WARNING: line, a blank line, and gt's
unprefixed remediation, which is why the echo reports stderr whole rather than
the severity lines alone.

The remote is a local bare repo; the owner/name in .graphite_repo_config points
Graphite's resolution at a real repository so the run reaches the fast-forward
phase at all. Nothing is pushed there.
