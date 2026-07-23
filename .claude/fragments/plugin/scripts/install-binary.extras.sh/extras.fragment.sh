# Sourced by install-binary.sh (the cc-skills shared fragment) after TAG/BARE
# and before any arm runs; redefining extras_on_exit() joins the installer's
# EXIT trap.

# migration (2026-07-04): overlay plugin updates keep the pre-v0.5.0 bootstrap
# shim at bin/ccx; a regular file with a #! header there is that shim, not a
# managed symlink — without this sniff its dev fallback parks it in the dev arm
# forever. The shim also cached versioned ccx-v* payloads; sweep those.
if [ -f "$LINK" ] && [ ! -L "$LINK" ] && [ "$(head -c 2 "$LINK" 2>/dev/null)" = "#!" ]; then
  rm -f "$LINK"
fi
rm -f "$DATA_DIR/bin/$NAME-v"* "${XDG_CACHE_HOME:-$HOME/.cache}/cc-context/bin/$NAME-v"*

# Best-effort: ensure ripgrep is available for `ccx code grep -i/-w`. Runs off
# the EXIT trap, backgrounded, so it never blocks session start and never races
# a foreground `brew install ccx` arm on Homebrew's global lock. ccx falls back
# to system grep when rg is absent, and no brew is not an error.
ensure_rg() {
  command -v rg >/dev/null 2>&1 && return 0
  command -v brew >/dev/null 2>&1 || return 0
  brew install ripgrep >/dev/null 2>&1 || true
}
extras_on_exit() { ensure_rg & }
