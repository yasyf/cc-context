"""cc-context web PDF driver.

Raw PDF bytes arrive on stdin (read to EOF); the parsed markdown leaves on
stdout. liteparse chatters progress to stdout, so fd 1 is pointed at devnull
behind a private handle before liteparse is imported and the response is
written to that handle. Any failure tracebacks to stderr and exits 1.
"""

import os
import sys

# Protect the response stream before anything else can touch fd 1: keep a
# private handle on the real stdout, then point fd 1 at devnull so liteparse's
# progress chatter cannot corrupt the response.
PROTO = os.fdopen(os.dup(1), "w")
_devnull = os.open(os.devnull, os.O_WRONLY)
os.dup2(_devnull, 1)
os.close(_devnull)

data = sys.stdin.buffer.read()
if not data:
    sys.stderr.write("pdf driver: empty stdin\n")
    sys.exit(1)

from liteparse import LiteParse

result = LiteParse(output_format="markdown", quiet=True).parse(data)
PROTO.write(result.text)
PROTO.flush()
