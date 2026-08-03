gt submit with a valid token against a repo Graphite cannot resolve — the remote
is a local bare repo, so nothing is pushed anywhere.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 1, with a WARNING: line ahead of the
ERROR:. This is what classifyGTSubmit's default arm faces: a real refusal in
none of the recognized wordings, wrapped verbatim. It also shows why the
recognized submit refusals below cannot be recorded — Graphite resolves the repo
through its API before gt looks at the stack, so a refusal about the stack is
downstream of a repo Graphite already accepted.

gt names the repo it could not verify after the bare remote's path, so these
bytes carry the recorder's work root (CCX_GT_RECORD_ROOT, default
/tmp/ccx-gt-record) and move with it.
