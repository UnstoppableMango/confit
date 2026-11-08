# A home-manager generation that uses confit's module, the way a desktop
# would: confit captures what the shared manifest tracks on this host and
# home-manager applies it. Built by run.sh from the flake's inputs, once per
# host.
{
  pkgs,
  homeManagerConfiguration, # home-manager.lib.homeManagerConfiguration
  module, # confit's homeManagerModules.default
  confit,
  user,
  home,
  host,
  remote,
}:
let
  hm = homeManagerConfiguration {
    inherit pkgs;
    modules = [
      module
      {
        home.username = user;
        home.homeDirectory = home;
        home.stateVersion = "25.11";
        services.confit = {
          enable = true;
          package = confit;
          inherit host remote;
        };
        # Also declared in Nix: confit records it, and a UI change to it.
        dconf.settings."org/gnome/desktop/wm/preferences".button-layout = "appmenu:close";
      }
    ];
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
