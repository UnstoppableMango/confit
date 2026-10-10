#!/usr/bin/env bash
# Real home-manager switches with confit's module, in a throwaway HOME on a
# private D-Bus. Run from the repo root inside `nix develop`.
set -euo pipefail
cd "$(dirname "$0")/../.."
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
home=$root/home
mkdir -p "$home/.local/state/nix/profiles"

build() {
  nix build --impure --no-link --print-out-paths --expr "
    let
      flake = builtins.getFlake (toString ./.);
      system = builtins.currentSystem;
    in
    (import ./e2e/home-manager/generation.nix {
      pkgs = flake.inputs.nixpkgs.legacyPackages.\${system};
      module = flake.homeManagerModules.default;
      confit = flake.packages.\${system}.default;
      user = \"$USER\";
      home = \"$home\";
    }).$1"
}
generation=$(build generation)
gsettings=$(build gsettings)

env -i HOME="$home" USER="$USER" PATH="$gsettings/bin:$PATH" TERM=dumb \
  dbus-run-session -- bash e2e/home-manager/scenario.sh "$generation"
