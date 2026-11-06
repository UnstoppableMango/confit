# Home Manager module: home-manager is a confit consumer. Every switch runs
# `confit apply`, which first captures live changes (drift), applies the
# consumer's drift policy, writes the integration branch to the live systems
# and records what was applied. Between switches, one `confit watch --all`
# service and a timer trigger captures.
#
# What is captured and applied lives in the repo's manifest, confit.toml,
# shared by every host (docs/multi-host.md). Declare adapters there once
# with `confit adapter add`; this module only says where the repo is.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib)
    escapeShellArg
    literalExpression
    mkEnableOption
    mkIf
    mkMerge
    mkOption
    mkRemovedOptionModule
    optionalString
    types
    ;

  cfg = config.services.confit;
  bin = lib.getExe cfg.package;
  confit = "${bin} -C ${escapeShellArg cfg.repo}";
  path = lib.makeBinPath (cfg.extraPackages ++ [ cfg.package ]);
  serviceEnv = [ "PATH=${path}" ];

  removed =
    name:
    mkRemovedOptionModule [ "services" "confit" name ] ''
      Adapters, consumers and drift policies now live in the repo's
      manifest, confit.toml, shared by every host. Declare them once with
      `confit adapter add` and `confit consumer add`; see docs/multi-host.md.
    '';
in
{
  imports = map removed [
    "adapters"
    "vscode"
    "dconf"
    "drift"
    "consumer"
    "integrationBranch"
  ];

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
      description = "This host's confit repository. Cloned from `remote`, or created empty, if missing.";
    };

    remote = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "git@github.com:me/desktop-config.git";
      description = ''
        The repository every host shares, as a git URL. It is cloned on the
        first switch and becomes the `origin` remote; confit fetches from it
        before capturing or applying and pushes afterwards. Unreachable, it
        is a warning: work is pushed later. Null keeps the repo local.
      '';
    };

    host = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = ''
        This host's name in the manifest (`confit.host`). Null uses the
        hostname. The consumer is `home-manager@<host>`.
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

    watch = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Capture soon after each change, with one `confit watch --all` user
        service following every dconf and file adapter this host tracks.
        Without it, changes are still captured by the timer and before
        every switch.
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
  };

  config = mkIf cfg.enable (mkMerge [
    {
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
        confitConsumer() {
          echo "home-manager@$(${confit} host)"
        }
        export PATH=${path}:$PATH
        ${optionalString (cfg.remote != null) ''
          if [[ ! -e ${escapeShellArg cfg.repo} ]]; then
            run mkdir -p "$(dirname ${escapeShellArg cfg.repo})"
            # Without the shared history there is nothing to capture into
            # or apply; try again on the next switch.
            if ! run git clone --quiet --bare ${escapeShellArg cfg.remote} ${escapeShellArg cfg.repo}; then
              warnEcho "confit: could not clone ${cfg.remote}; skipping confit until it is reachable"
              confitSkip=1
            fi
          fi
        ''}
        if [[ ! -v confitSkip ]]; then
          run ${bin} init --bare ${escapeShellArg cfg.repo}
          ${optionalString (
            cfg.host != null
          ) "run git -C ${escapeShellArg cfg.repo} config confit.host ${escapeShellArg cfg.host}"}
          ${optionalString (
            cfg.remote != null
          ) "run git -C ${escapeShellArg cfg.repo} config remote.origin.url ${escapeShellArg cfg.remote}"}
          # home-manager applies every adapter this host tracks, unless the
          # manifest already says otherwise.
          run confitRun consumer add "$(confitConsumer)" --adapter '*' --if-missing
          run confitRun capture --all || true
        fi
      '';

      # After dconfSettings, so keys set in `dconf.settings` are captured as
      # drift (adopted by default) rather than reverted on the next switch.
      home.activation.confit = mkIf cfg.apply (
        lib.hm.dag.entryAfter [ "writeBoundary" "dconfSettings" "confitCapture" ] ''
          if [[ -v DRY_RUN ]]; then
            echo "Would run confit apply"
          elif [[ -v confitSkip ]]; then
            :
          elif confitRun apply "$(confitConsumer)"; then
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
    }

    (mkIf (cfg.captureInterval != null) {
      systemd.user.services.confit-capture-all = {
        Unit.Description = "Capture every confit adapter";
        Service = {
          Type = "oneshot";
          Environment = serviceEnv;
          ExecStart = "${confit} capture --all";
          # Exit 3 is a conflict: recorded and waiting for a human, not a failure.
          SuccessExitStatus = "3";
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

    (mkIf cfg.watch {
      # One small trigger for everything: `dconf watch` and inotify for what
      # the manifest tracks on this host, reloaded when the manifest changes.
      systemd.user.services.confit-watch = {
        Unit = {
          Description = "Capture live changes into confit";
          PartOf = [ "graphical-session.target" ];
          After = [ "graphical-session.target" ];
        };
        Service = {
          Environment = serviceEnv;
          ExecStart = "${confit} watch --all --quiet ${cfg.quiet}";
          Restart = "on-failure";
          RestartSec = 5;
        };
        Install.WantedBy = [ "graphical-session.target" ];
      };
    })
  ]);
}
