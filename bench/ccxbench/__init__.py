"""Cache-aware cost-per-correct-answer benchmark for cc-context (ccx).

Two arms (baseline vs ccx) run the same tasks through real `claude -p` headless
sessions; the harness reads the cache-aware cost Claude Code already computes and
pairs it with a deterministic task-success grade. The deliverable is
cost-per-correct-answer and accuracy per model — never a raw "tokens saved" number.
"""
