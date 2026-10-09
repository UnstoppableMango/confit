{
  description = "Use git as a buffer and ledger between config UIs and the systems they configure";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
    systems.url = "github:UnstoppableMango/nix-systems";

    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.inputs.systems.follows = "systems";
    };
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = import inputs.systems;
      imports = with inputs; [ treefmt-nix.flakeModule ];

      perSystem =
        { pkgs, system, ... }:
        let
          version = "0.0.1";
        in
        {
          _module.args.pkgs = import inputs.nixpkgs {
            inherit system;
            overlays = with inputs; [ gomod2nix.overlays.default ];
          };

          packages.default = pkgs.callPackage ./nix { inherit version; };

          devShells.default = pkgs.mkShellNoCC {
            packages = with pkgs; [
              dbus
              dconf
              direnv
              git
              go
              gomod2nix
              gopls
              gnumake
              nixfmt
            ];

            GO = "${pkgs.go}/bin/go";
            GOMOD2NIX = "${pkgs.gomod2nix}/bin/gomod2nix";
          };

          treefmt.programs = {
            actionlint.enable = true;
            nixfmt.enable = true;
            gofmt.enable = true;
          };
        };
    };
}
