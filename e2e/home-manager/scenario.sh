#!/usr/bin/env bash
# Driven by run.sh: HOME is a throwaway, the D-Bus session is private.
# Each step does what a person would, then checks git and the live system.
set -euo pipefail
generation=$1
confit=$generation/home-path/bin/confit
repo=$HOME/.local/share/confit/repo.git
settings=$HOME/.config/Code/User/settings.json
editor=dconf@$(hostname)
c() { "$confit" -C "$repo" "$@"; }
switch() { "$generation/activate" >"$HOME/switch.log" 2>&1 || { cat "$HOME/switch.log"; exit 1; }; }
step() { printf '\n### %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
expect() { [[ $2 == "$3" ]] || fail "$1: got $2, want $3"; }
# has TEXT NEEDLE MESSAGE; not `| grep -q`, which fails under pipefail when
# grep exits before the writer is done.
has() { [[ $1 == *"$2"* ]] || fail "$3"; }

step "An existing desktop, before confit"
mkdir -p "$(dirname "$settings")"
cat >"$settings" <<'J'
{
  // laptop
  "editor.fontSize": 14,
  "workbench.colorTheme": "Default Dark Modern",
}
J
gsettings set org.gnome.desktop.interface color-scheme prefer-dark

step "First switch records it and applies"
switch
c status
expect "applied" "$(git -C "$repo" rev-parse applied/home-manager@e2e)" "$(git -C "$repo" rev-parse desired)"
has "$(git -C "$repo" show desired:vscode/settings.json)" '// laptop' "comment lost"
has "$(git -C "$repo" show desired:dconf/desktop/interface.ini)" "color-scheme='prefer-dark'" "dconf not captured"

step "VSCode and Tweaks edits, captured as the systemd units would"
sed -i 's/"editor.fontSize": 14/"editor.fontSize": 15/' "$settings"
c capture vscode # confit-capture@vscode.service, started by the .path unit
"$confit" -C "$repo" watch dconf --quiet 300ms & # confit-watch-dconf.service
watcher=$!
sleep 1
gsettings set org.gnome.desktop.interface clock-show-seconds true
gsettings set org.gnome.desktop.wm.preferences focus-mode sloppy
sleep 1.5
kill -TERM "$watcher"
wait "$watcher" || true
has "$(git -C "$repo" log --format=%s desired)" 'vscode: set editor.fontSize = 15' "vscode edit not committed"
has "$(git -C "$repo" log --format=%s -1 "edits/$editor")" '2 changes' "dconf burst not one commit"

step "A change while nothing watches is adopted by the next switch"
gsettings set org.gnome.desktop.interface enable-hot-corners false
switch
expect "hot corners" "$(gsettings get org.gnome.desktop.interface enable-hot-corners)" false
has "$(cat "$settings")" '"editor.fontSize": 15' "font size not kept"
has "$(git -C "$repo" notes --ref=confit-applied show applied/home-manager@e2e)" 'Result: ok' "no apply note"

step "Tweaks changes a key home-manager declares: recorded, then Nix wins"
gsettings set org.gnome.desktop.wm.preferences button-layout 'close,minimize:'
switch
expect "button-layout" "$(gsettings get org.gnome.desktop.wm.preferences button-layout)" "'appmenu:close'"
has "$(git -C "$repo" log --format=%s "edits/$editor")" "button-layout = 'close,minimize:'" "UI change not recorded before the switch overwrote it"

step "A switch with nothing new moves no branch"
before=$(git -C "$repo" for-each-ref refs/heads)
switch
expect "branches" "$(git -C "$repo" for-each-ref refs/heads)" "$before"

step "History"
git -C "$repo" log --format='%h %s' desired
echo PASS
