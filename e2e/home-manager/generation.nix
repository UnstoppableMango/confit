# A home-manager generation that uses confit's module, the way a desktop
# would: VSCode settings and GNOME's dconf captured, home-manager applying.
# Built by run.sh; takes its pieces as arguments so it needs no flake inputs
# of its own.
{
  pkgs,
  module, # confit's homeManagerModules.default
  confit,
  user,
  home,
}:
let
  hm = import "${pkgs.home-manager.src}/modules" {
    inherit pkgs;
    configuration = {
      imports = [ module ];
      home.username = user;
      home.homeDirectory = home;
      home.stateVersion = "25.11";
      services.confit = {
        enable = true;
        package = confit;
        consumer = "home-manager@e2e";
        vscode.enable = true;
        dconf.enable = true;
      };
      # Also declared in Nix: confit records it, and a UI change to it.
      dconf.settings."org/gnome/desktop/wm/preferences".button-layout = "appmenu:close";
    };
  };
in
{
  generation = hm.activationPackage;
  # GSettings as GNOME Settings and Tweaks use it: GNOME's schemas and the
  # dconf backend.
  gsettings = pkgs.writeShellScriptBin "gsettings" ''
    export GSETTINGS_SCHEMA_DIR=${pkgs.gsettings-desktop-schemas}/share/gsettings-schemas/${pkgs.gsettings-desktop-schemas.name}/glib-2.0/schemas
    export GIO_EXTRA_MODULES=${pkgs.dconf.lib}/lib/gio/modules
    exec ${pkgs.glib.bin}/bin/gsettings "$@"
  '';
}
