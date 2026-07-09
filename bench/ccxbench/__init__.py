"""Token-usage benchmark for cc-context (ccx).

Three arms (baseline, ccx-mcp, ccx-cli) run the same tasks through real
`claude -p` headless sessions. The harness pairs deterministic task-success grades
with transcript and envelope token metrics, reporting accuracy-gated savings per
model and ccx arm.
"""
