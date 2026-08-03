NOT RECORDED — settled, awaiting capture.

gt sync that writes a diagnostic to stderr and exits 0 anyway. `gtZeroSurfaces`
and restack.go's zero-surfaces path both turn on this behavior existing.

The dispute is resolved: two measurements of gt 1.8.6 disagreed because they
were about **different messages**.

- `ERROR: Cannot pull trunk due to conflicting unstaged changes. ` exits **1**,
  universally — 19 configurations including `--force`, `-q`, `-a`, and a real
  PTY. The exit-0 pairing once recorded for it was false.
- `WARNING: <branch> could not be restacked cleanly.` exits **0**, on a plain
  `gt sync --no-interactive` with no `--force`, while stdout's restack section
  stays empty. This is the real case, and it recurs: 56 of 9,346 real gt run
  logs carry it.

So the zero-surfaces path is live and correctly justified — the doc comment that
cited the trunk-pull ERROR was pointing at the wrong message, not defending a
behavior that does not exist. cc-notes `db6d174` records both exit codes and
flags the superseded claim.

Bytes for this shape have been captured (`WARNING:` + blank line + gt's
`Please resolve conflicts in the current stack with gt restack.` remediation)
and are held pending a recorder scenario, so this stays empty only until that
write lands — not because anything is unknown.
