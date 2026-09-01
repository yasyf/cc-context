gt submit with no Graphite token.

Recorded offline, no token. Exit 1. Pins gtAuthRequired1 ("Please
authenticate your Graphite CLI"), which classifyGTSubmit turns into the gt auth
advice for stack submit; ship's own submit speaks to Graphite's HTTP API and
classifies its typed errors instead.
