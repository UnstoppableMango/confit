#!/usr/bin/env bash
# Driven by run.sh: two hosts, each with a throwaway HOME, sharing $REMOTE.
# Checks what only real switches can: the module's activation steps and
# hosts syncing through the remote. The desktop scenarios themselves are in
# e2e/e2e_test.go.
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
# Not `| grep -q`, which fails under pipefail when grep exits early.
has() { [[ $1 == *"$2"* ]] || fail "$3"; }
lacks() { [[ $1 != *"$2"* ]] || fail "$3"; }

# on HOST CMD...: run CMD as HOST, in its HOME on a private D-Bus.
on() {
  local host=$1
  shift
  env HOME="$ROOT/$host" XDG_RUNTIME_DIR="$ROOT/run-$host" scripts/dbus-session.sh "$@"
}
generation() { if [[ $1 == laptop ]]; then echo "$LAPTOP"; else echo "$DESKTOP"; fi; }
switch() {
  on "$1" "$(generation "$1")/activate" >"$ROOT/switch.log" 2>&1 || { cat "$ROOT/switch.log"; exit 1; }
}
repo() { echo "$ROOT/$1/.local/share/confit/repo.git"; }
confit() { local host=$1; shift; on "$host" "$(generation "$host")/home-path/bin/confit" -C "$(repo "$host")" "$@"; }
settings() { echo "$ROOT/$1/.config/Code/User/settings.json"; }

echo "### The first switch clones the empty remote and registers the host"
switch laptop
has "$(git -C "$REMOTE" show desired:confit.toml)" 'home-manager@laptop' "laptop's consumer not pushed"

echo "### Adapters are declared once, in the shared manifest"
confit laptop adapter add vscode --type file --file 'settings.json=~/.config/Code/User/settings.json' >/dev/null
confit laptop adapter add dconf --type dconf --root /org/gnome/ >/dev/null
mkdir -p "$(dirname "$(settings laptop)")"
echo '{"editor.fontSize": 14}' >"$(settings laptop)"
on laptop gsettings set org.gnome.desktop.interface color-scheme prefer-dark

echo "### A switch records the existing desktop and applies"
switch laptop
laptop=$(repo laptop)
[[ $(git -C "$laptop" rev-parse applied/home-manager@laptop) == $(git -C "$laptop" rev-parse desired) ]] || fail "applied pointer not at desired"
has "$(git -C "$REMOTE" show desired:dconf/desktop/interface.ini)" "color-scheme='prefer-dark'" "live dconf not captured and pushed"
has "$(git -C "$REMOTE" show desired:vscode/settings.json)" '"editor.fontSize": 14' "settings.json not captured and pushed"

echo "### A UI change to a key home-manager declares is recorded before the switch overwrites it"
on laptop gsettings set org.gnome.desktop.wm.preferences button-layout 'close,minimize:'
switch laptop
[[ $(on laptop gsettings get org.gnome.desktop.wm.preferences button-layout) == "'appmenu:close'" ]] || fail "home-manager's value didn't win"
has "$(git -C "$laptop" log --format=%s edits/dconf@laptop)" "button-layout = 'close,minimize:'" "UI change lost unrecorded"

echo "### A switch with nothing new moves no branch"
before=$(git -C "$laptop" for-each-ref refs/heads)
switch laptop
[[ $(git -C "$laptop" for-each-ref refs/heads) == "$before" ]] || fail "a no-op switch moved branches"

echo "### A second host clones the remote and gets the shared settings"
switch desktop
[[ $(on desktop gsettings get org.gnome.desktop.interface color-scheme) == "'prefer-dark'" ]] || fail "shared dconf key not applied on the desktop"
has "$(cat "$(settings desktop)")" '"editor.fontSize": 14' "shared settings.json not applied on the desktop"

echo "### A local change stays on its host; a shared one reaches the other"
confit desktop adapter add vscode --type file --file 'settings.json=~/.config/Code/User/settings.json' \
  --local settings.json:window.zoomLevel >/dev/null
echo '{"editor.fontSize": 16, "window.zoomLevel": 2}' >"$(settings desktop)"
switch desktop
has "$(git -C "$REMOTE" show desired:hosts/desktop/vscode/settings.json)" '"window.zoomLevel": 2' "local key not in the desktop's layer"
lacks "$(git -C "$REMOTE" show desired:vscode/settings.json)" 'zoomLevel' "local key leaked into the shared layer"
switch laptop
has "$(cat "$(settings laptop)")" '"editor.fontSize": 16' "the desktop's shared change didn't reach the laptop"
lacks "$(cat "$(settings laptop)")" 'zoomLevel' "the desktop's local key reached the laptop"
has "$(git -C "$REMOTE" notes --ref=confit-applied/laptop show applied/home-manager@laptop)" 'Consumer: home-manager@laptop' "laptop's apply note not pushed"
echo PASS
