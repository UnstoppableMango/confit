#!/usr/bin/env bash
# Runs all tests, including the dconf ones, against a throwaway dconf
# database on a private D-Bus session. Needs git >= 2.42, dconf-cli, dbus.
set -euo pipefail
cd "$(dirname "$0")/.."
export XDG_CONFIG_HOME
XDG_CONFIG_HOME=$(mktemp -d)
trap 'rm -rf "$XDG_CONFIG_HOME"' EXIT
CONFIT_DCONF_TESTS=1 dbus-run-session -- go test "$@" ./...
