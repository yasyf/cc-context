A clean no-op gt sync with tips on: nothing to fast-forward, nothing to restack.

Recorded live (CCX_GT_RECORD_TOKEN). Exit 0 with **stderr non-empty and no
severity line on it** — the negative case gtResult.Diagnostics' gate exists for.
Without that gate this stderr would be reported as a diagnostic on every
ordinary ship, because gt's NUX tips are unprefixed stderr exactly as the
remediation is. Pair with sync-quiet-exit0, the same sync with tips off.

TIP BYTES ARE A FUNCTION OF THE WHOLE SCENARIO, NOT THE CAPTURED COMMAND. gt
shows each nux a fixed number of times per HOME and records the count in
$XDG_DATA_HOME/graphite/nuxes: init.welcome and tip.expert-message cap at 1 and
are both spent by the gt init graphite_repo runs, runner.undo caps at 3. So the
dots in [runner.undo ●○○] say "first showing", and tip.expert-message is absent
here because gt init already used its only one. Add or remove a gt command
anywhere in this scenario's setup and the dots move. A dot mismatch on a
re-record means the setup changed — it is not licence to edit the expected
bytes.
