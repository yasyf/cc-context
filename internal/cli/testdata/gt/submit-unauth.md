gt submit with no Graphite token.

Recorded offline, no token. Exit 1. The argv is gtSubmitArgv's own, so the
golden pins the flags ccx passes as well as the refusal it gets back. Pins
gtAuthRequired1 ("Please authenticate your Graphite CLI"), which classifyGTSubmit
turns into the gt auth advice.
