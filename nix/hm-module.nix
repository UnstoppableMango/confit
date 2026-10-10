# Home Manager module: home-manager is a confit consumer. Every switch runs
# `confit apply`, which first captures live changes (drift), applies the
# consumer's drift policy, writes the integration branch to the live systems
# and records what was applied. Between switches, systemd triggers captures:
# a .path unit per file adapter and `confit watch` for dconf.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib)
    concatMap
    concatStringsSep
    escapeShellArg
    escapeShellArgs
    filterAttrs
    literalExpression
    mapAttrs'
    mapAttrsToList
    mkEnableOption
    mkIf
    mkMerge
    mkOption
    nameValuePair
    optional
    types
    ;

  cfg = config.services.confit;
  confit = "${lib.getExe cfg.package} -C ${escapeShellArg cfg.repo}";

  adapterArgs =
    name: a:
    [
      name
      "--type"
      a.type
    ]
    ++ optional (a.path != null) "--path=${a.path}"
    ++ optional (a.root != null) "--root=${a.root}"
    ++ optional (a.command != null) "--command=${a.command}"
    ++ concatMap (f: [ "--file=${f}" ]) a.files;

  adapterOpts =
    { name, ... }:
    {
      options = {
        type = mkOption {
          type = types.str;
          description = "`file`, `dconf`, or the name of an external `confit-adapter-<type>`.";
        };
        files = mkOption {
          type = types.listOf types.str;
          default = [ ];
          description = "file adapter: `[NAME=]PATH` entries, NAME being the file's name in the repo.";
        };
        root = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = "dconf adapter: the subtree to manage, e.g. `/org/gnome/`.";
        };
        path = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = "Directory in the repo. Defaults to the adapter's name.";
        };
        command = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = "External adapter executable.";
        };
        watch = mkOption {
          type = types.bool;
          default = true;
          description = ''
            Capture soon after each change. File adapters use a systemd path
            unit; dconf runs `confit watch`, a small stateless trigger.
            Without it, changes are still captured by the timer and before
            every switch.
          '';
        };
      };
    };

  watched = filterAttrs (_: a: a.watch) cfg.adapters;
  fileWatched = filterAttrs (_: a: a.type == "file") watched;
  procWatched = filterAttrs (_: a: a.type == "dconf") watched;
  livePath = spec: lib.last (lib.splitString "=" spec);

  serviceEnv = [ "PATH=${lib.makeBinPath (cfg.extraPackages ++ [ cfg.package ])}" ];
in
{
  options.services.confit = {
    enable = mkEnableOption "confit, which records configuration changes made in UIs as git commits";

    package = mkOption {
      type = types.package;
      description = "The confit package.";
    };

    extraPackages = mkOption {
      type = types.listOf types.package;
      default = [
        pkgs.git
        pkgs.dconf
      ];
      defaultText = literalExpression "[ pkgs.git pkgs.dconf ]";
      description = "Tools confit's commands and adapters run.";
    };

    repo = mkOption {
      type = types.str;
      default = "${config.xdg.dataHome}/confit/repo.git";
      defaultText = literalExpression ''"''${config.xdg.dataHome}/confit/repo.git"'';
      description = "The confit repository. Created as a bare repository if missing.";
    };

    integrationBranch = mkOption {
      type = types.str;
      default = "desired";
      description = "Branch editors integrate into and this consumer applies.";
    };

    consumer = mkOption {
      type = types.str;
      default = "home-manager";
      description = "This consumer's name; its applied pointer is `applied/<name>`.";
    };

    drift = mkOption {
      type = types.enum [
        "adopt"
        "revert"
        "block"
      ];
      default = "adopt";
      description = ''
        What a switch does with live changes it finds: keep them (`adopt`),
        record and overwrite them (`revert`), or refuse to apply until someone
        decides (`block`).
      '';
    };

    apply = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Apply the integration branch on every switch. When the drift policy
        blocks or a conflict needs resolving, the switch carries on and
        prints a warning; nothing is overwritten.
      '';
    };

    quiet = mkOption {
      type = types.str;
      default = "2s";
      description = "How long a watched system must be unchanged before a capture runs.";
    };

    captureInterval = mkOption {
      type = types.nullOr types.str;
      default = "hourly";
      description = "systemd calendar expression for capturing every adapter, or null.";
    };

    vscode = {
      enable = mkEnableOption "capturing VSCode's user settings.json";
      settingsFile = mkOption {
        type = types.str;
        default = "${config.xdg.configHome}/Code/User/settings.json";
        defaultText = literalExpression ''"''${config.xdg.configHome}/Code/User/settings.json"'';
        description = "Live settings.json. Don't also set `programs.vscode.userSettings`: confit owns this file.";
      };
    };

    dconf = {
      enable = mkEnableOption "capturing dconf (GNOME Settings, Tweaks, any GSettings app)";
      root = mkOption {
        type = types.str;
        default = "/org/gnome/";
        description = "dconf subtree to manage.";
      };
    };

    adapters = mkOption {
      type = types.attrsOf (types.submodule adapterOpts);
      default = { };
      example = literalExpression ''
        {
          kitty = { type = "file"; files = [ "kitty.conf=/home/me/.config/kitty/kitty.conf" ]; };
        }
      '';
      description = "Adapters this consumer applies through.";
    };
  };

  config = mkIf cfg.enable (mkMerge [
    {
      services.confit.adapters = mkMerge [
        (mkIf cfg.vscode.enable {
          vscode = {
            type = "file";
            files = [ "settings.json=${cfg.vscode.settingsFile}" ];
          };
        })
        (mkIf cfg.dconf.enable {
          dconf = {
            type = "dconf";
            inherit (cfg.dconf) root;
          };
        })
      ];

      home.packages = [ cfg.package ];
      home.sessionVariables.CONFIT_REPO = cfg.repo;

      # Capture before home-manager writes anything (including the keys in
      # `dconf.settings`), so a UI change that the switch is about to
      # overwrite is still recorded.
      home.activation.confitCapture = lib.hm.dag.entryBefore [ "writeBoundary" ] ''
        confitRun() {
          # dconf needs a session bus; mirror home-manager's own dconf step.
          if [[ -v DBUS_SESSION_BUS_ADDRESS ]]; then
            ${confit} "$@"
          else
            ${pkgs.dbus}/bin/dbus-run-session --dbus-daemon=${pkgs.dbus}/bin/dbus-daemon -- ${confit} "$@"
          fi
        }
        export PATH=${lib.makeBinPath (cfg.extraPackages ++ [ cfg.package ])}:$PATH
        run ${lib.getExe cfg.package} init --bare --integration ${escapeShellArg cfg.integrationBranch} ${escapeShellArg cfg.repo}
        ${concatStringsSep "\n" (
          mapAttrsToList (n: a: "run ${confit} adapter add ${escapeShellArgs (adapterArgs n a)}") cfg.adapters
        )}
        run ${confit} consumer add ${escapeShellArg cfg.consumer} --drift ${cfg.drift} ${
          escapeShellArgs (concatMap (n: [ "--adapter=${n}" ]) (builtins.attrNames cfg.adapters))
        }
        ${concatStringsSep "\n" (
          map (n: "run confitRun capture ${escapeShellArg n} || true") (builtins.attrNames cfg.adapters)
        )}
      '';

      # After dconfSettings, so keys set in `dconf.settings` are captured as
      # drift (adopted by default) rather than reverted on the next switch.
      home.activation.confit = mkIf cfg.apply (
        lib.hm.dag.entryAfter [ "writeBoundary" "dconfSettings" "confitCapture" ] ''
          if [[ -v DRY_RUN ]]; then
            echo "Would run confit apply ${cfg.consumer}"
          elif confitRun apply ${escapeShellArg cfg.consumer}; then
            :
          else
            code=$?
            if (( code == 3 )); then
              warnEcho "confit: not applied (drift policy or a conflict needs a decision); see: confit status"
            else
              warnEcho "confit apply failed with exit code $code"
            fi
          fi
        ''
      );

      systemd.user.services."confit-capture@" = {
        Unit.Description = "Capture %i's live state into confit";
        Service = {
          Type = "oneshot";
          Environment = serviceEnv;
          ExecStart = "${confit} capture %i";
          # Exit 3 is a conflict: recorded and waiting for a human, not a failure.
          SuccessExitStatus = "3";
        };
      };
    }

    (mkIf (cfg.captureInterval != null && cfg.adapters != { }) {
      systemd.user.services.confit-capture-all = {
        Unit.Description = "Capture every confit adapter";
        Service = {
          Type = "oneshot";
          Environment = serviceEnv;
          ExecStart = map (n: "-${confit} capture ${escapeShellArg n}") (builtins.attrNames cfg.adapters);
        };
      };
      systemd.user.timers.confit-capture-all = {
        Unit.Description = "Capture every confit adapter periodically";
        Timer = {
          OnCalendar = cfg.captureInterval;
          OnStartupSec = "1min";
          Persistent = true;
        };
        Install.WantedBy = [ "timers.target" ];
      };
    })

    {
      # No process of ours: systemd's inotify starts a one-shot capture.
      systemd.user.paths = mapAttrs' (
        n: a:
        nameValuePair "confit-capture-${n}" {
          Unit.Description = "Watch ${n}'s files for confit";
          Path = {
            PathChanged = map livePath a.files;
            Unit = "confit-capture@${n}.service";
          };
          Install.WantedBy = [ "default.target" ];
        }
      ) fileWatched;

      # dconf has no file worth watching, so a tiny trigger follows
      # `dconf watch` and runs a capture after each quiet period.
      systemd.user.services = mapAttrs' (
        n: _:
        nameValuePair "confit-watch-${n}" {
          Unit = {
            Description = "Capture ${n} changes into confit";
            PartOf = [ "graphical-session.target" ];
            After = [ "graphical-session.target" ];
          };
          Service = {
            Environment = serviceEnv;
            ExecStart = "${confit} watch ${escapeShellArg n} --quiet ${cfg.quiet}";
            Restart = "on-failure";
            RestartSec = 5;
          };
          Install.WantedBy = [ "graphical-session.target" ];
        }
      ) procWatched;
    }
  ]);
}
