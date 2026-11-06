#!/usr/bin/env bash
# Real home-manager switches with confit's module on two hosts, each with
# its own HOME and private D-Bus, sharing one bare remote. Run from the repo
# root inside `nix develop`.
set -euo pipefail
cd "$(dirname "$0")/../.."
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
remote=$root/remote.git
git init --quiet --bare --initial-branch=desired "$remote"

build() { # host attr
  nix build --impure --no-link --print-out-paths --expr "
    let
      flake = builtins.getFlake (toString ./.);
      system = builtins.currentSystem;
    in
    (import ./e2e/home-manager/generation.nix {
      pkgs = flake.inputs.nixpkgs.legacyPackages.\${system};
      inherit (flake.inputs.home-manager.lib) homeManagerConfiguration;
      module = flake.homeManagerModules.default;
      confit = flake.packages.\${system}.default;
      user = \"$USER\";
      home = \"$root/$1\";
      host = \"$1\";
      remote = \"$remote\";
    }).$2"
}
for host in laptop desktop; do
  mkdir -p "$root/$host/.local/state/nix/profiles"
  # An empty XDG_RUNTIME_DIR keeps the switch away from the machine's real
  # systemd user manager, which would be asked to start this HOME's units.
  mkdir -m 700 "$root/run-$host"
done
gsettings=$(build laptop gsettings)

env -i ROOT="$root" REMOTE="$remote" USER="$USER" PATH="$gsettings/bin:$PATH" TERM=dumb \
  LAPTOP="$(build laptop generation)" DESKTOP="$(build desktop generation)" \
  bash e2e/home-manager/scenario.sh
