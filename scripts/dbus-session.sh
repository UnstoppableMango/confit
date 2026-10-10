#!/usr/bin/env bash
# Runs a command on a private D-Bus session that can start dconf, using the
# dbus and dconf from PATH. Nix's dbus-run-session otherwise looks for
# /etc/dbus-1/session.conf, which only exists on NixOS, and Nix's dconf
# service isn't in any standard service directory.
set -euo pipefail
daemon=$(command -v dbus-daemon)
dconf=$(command -v dconf)
conf=$(mktemp)
trap 'rm -f "$conf"' EXIT
cat >"$conf" <<XML
<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <servicedir>$(dirname "$dconf")/../share/dbus-1/services</servicedir>
  <include>$(dirname "$daemon")/../share/dbus-1/session.conf</include>
</busconfig>
XML
dbus-run-session --dbus-daemon="$daemon" --config-file="$conf" -- "$@"
