#!/usr/bin/env bash
# Driven by run.sh: HOME is a throwaway, the D-Bus session is private.
# Checks what only a real switch can: the module's activation steps. The
# desktop scenarios themselves are in e2e/e2e_test.go.
set -euo pipefail
generation=$1
repo=$HOME/.local/share/confit/repo.git
switch() { "$generation/activate" >"$HOME/switch.log" 2>&1 || { cat "$HOME/switch.log"; exit 1; }; }
fail() { echo "FAIL: $*" >&2; exit 1; }
# Not `| grep -q`, which fails under pipefail when grep exits early.
has() { [[ $1 == *"$2"* ]] || fail "$3"; }

echo "### First switch records the existing desktop and applies"
gsettings set org.gnome.desktop.interface color-scheme prefer-dark
switch
[[ $(git -C "$repo" rev-parse applied/home-manager@e2e) == $(git -C "$repo" rev-parse desired) ]] || fail "applied pointer not at desired"
has "$(git -C "$repo" show desired:dconf/desktop/interface.ini)" "color-scheme='prefer-dark'" "live dconf not captured"

echo "### A UI change to a key home-manager declares is recorded before the switch overwrites it"
gsettings set org.gnome.desktop.wm.preferences button-layout 'close,minimize:'
switch
[[ $(gsettings get org.gnome.desktop.wm.preferences button-layout) == "'appmenu:close'" ]] || fail "home-manager's value didn't win"
has "$(git -C "$repo" log --format=%s "edits/dconf@$(hostname)")" "button-layout = 'close,minimize:'" "UI change lost unrecorded"

echo "### A switch with nothing new moves no branch"
before=$(git -C "$repo" for-each-ref refs/heads)
switch
[[ $(git -C "$repo" for-each-ref refs/heads) == "$before" ]] || fail "a no-op switch moved branches"
echo PASS
