#!/usr/bin/env bash
# One-command benchmark. Builds the fixture + corpus, self-tests the graders (no API
# cost), then runs. With no argument it runs the pilot (a few tasks, 1 repeat) to
# validate the harness end to end before any paid full run.
#
#   ./run.sh                 # pilot
#   ./run.sh pilot --models sonnet
#   ./run.sh run             # full corpus at config.toml settings
#   ./run.sh run --categories navigation,callees --repeats 3
set -euo pipefail
cd "$(dirname "$0")"

python3 -m ccxbench build-corpus
python3 -m ccxbench selftest
python3 -m ccxbench crosscheck tests/data/haiku_envelope.json

exec python3 -m ccxbench "${@:-pilot}"
