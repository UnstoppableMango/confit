# confit

Confit records every configuration change a UI makes as a git commit, lets consumers (home-manager, NixOS, scripts) apply those commits later, and records what each consumer applied. Git is the only state. Every command is one-shot, so nothing needs a daemon.

Design: [docs/design.md](docs/design.md). Language choice: [docs/language-spike.md](docs/language-spike.md).

Status: step 4 of the plan. The core runs end to end against VSCode settings and GNOME (dconf) with home-manager as the consumer; see [docs/end-to-end.md](docs/end-to-end.md) for what ran and what it found.

## With home-manager

The flake exports a Home Manager module. home-manager becomes a consumer: every `home-manager switch` captures live changes first, applies the drift policy, writes the integration branch to the live systems and records the apply. Between switches, systemd starts captures: a `.path` unit for files, `confit watch` for dconf, and an hourly timer.

```nix
{
  imports = [ inputs.confit.homeManagerModules.default ];
  services.confit = {
    enable = true;
    vscode.enable = true;   # ~/.config/Code/User/settings.json
    dconf.enable = true;    # /org/gnome/
    # drift = "adopt";      # or "revert" / "block"
  };
}
```

Don't also set `programs.vscode.userSettings`: that makes `settings.json` a read-only Nix store link, and the settings UI can't save. Keys in `dconf.settings` keep working; a switch records a UI change to one of them before home-manager overwrites it.

## Quick start

```sh
confit init --bare ~/.local/share/confit/repo.git
export CONFIT_REPO=~/.local/share/confit/repo.git

confit adapter add vscode --type file --file settings.json=$HOME/.config/Code/User/settings.json
confit adapter add dconf  --type dconf --root /org/gnome/
confit consumer add home-manager@laptop --adapter vscode --adapter dconf --drift adopt

confit capture vscode        # snapshot the live file, commit any change, integrate it
confit pending home-manager@laptop
confit status
```

A consumer that applies by itself (home-manager) does:

```sh
target=$(confit prepare home-manager@laptop)   # captures drift first; exit 3 if the policy blocks
home-manager switch --flake ...#$target     # reads the repo at $target
confit applied advance home-manager@laptop "$target"
```

A consumer that confit can apply through its adapters runs `confit apply <consumer>`, which does all three steps.

## Commands

| Command | What it does |
|---|---|
| `confit init [--bare] [--integration NAME] [path]` | Creates the repo, registers the merge drivers in git config, and creates the integration branch with a `.gitattributes`. Run it on every clone, since git config isn't cloned. |
| `confit adapter add` / `confit consumer add` | Write `confit.adapter.*` / `confit.consumer.*` git config. |
| `confit capture <adapter>` | Snapshot live state, commit the difference to `edits/<editor>`, integrate. No difference means no commit. |
| `confit watch <adapter> [--quiet 2s]` | Optional trigger for dconf and file adapters: captures once, then runs `confit capture` after each burst of changes once the system has been quiet. Holds no state. |
| `confit edit commit <editor> --dir D --prefix P` | For UIs and extensions that hand confit their files directly. |
| `confit integrate [<editor>...]` | Merge editor branches into the integration branch. A conflicting editor is reported and left out; the others still integrate. |
| `confit pending <consumer>` | Commits in `applied/<consumer>..<source>` and the tree diff. |
| `confit prepare <consumer>` | Drift check: capture the consumer's adapters, apply its drift policy, print the commit to apply. |
| `confit apply <consumer> [--force]` | prepare, write the target tree through the adapters, advance. |
| `confit applied advance <consumer> <commit> [--expect OLD]` | Compare-and-swap the applied pointer and append an apply note. Refuses commits outside the source branch. |
| `confit status [--json]` | Unintegrated editors, conflicts, pending work, recorded drift. |
| `confit merge-driver json\|ini %O %A %B` | Invoked by git, never by hand. |

Every command takes `-C <repo>` (or `$CONFIT_REPO`). Most take `--json`. Exit codes: 0 ok, 1 error, 2 usage, 3 conflict or blocked.

## How it maps to the design

- **Refs**: `edits/<editor>`, the integration branch (`desired` by default, configurable, and HEAD is never consulted), `applied/<consumer>`, and `refs/notes/confit-applied`. All prefixes are git config.
- **Spike rules, all enforced in `internal/gitrepo`**:
  - go-git only reads and writes objects.
  - Every ref move is a `git update-ref --stdin` transaction.
  - Merges are `git merge-tree` with `attr.tree` set to the integration branch.
  - Notes are written by confit with compare-and-swap, never `git notes add`.
- **Merge drivers**: JSON/JSONC and dconf INI. Both try git's line merge first. If that conflicts, they merge key by key. JSONC edits are applied as a patch, so comments and formatting survive. A key changed on both sides leaves normal conflict markers for any mergetool.
- **Commit metadata**: author is the human from git config, committer is `confit (<adapter>)`. Trailers are `Confit-Editor`, `Confit-Adapter`, `Confit-Source`, `Confit-Session` and `Confit-Keys`. Summaries look like `vscode: set editor.fontSize = 14`.
- **Drift policies** (`adopt` default, `revert`, `block`) behave as the design describes. The tests cover each one.

## Decisions made while building (worth a look)

1. **An editor branch tracks the live state the adapter last saw.** After an apply, confit adds a sync commit (with the applied commit as a parent) to each affected editor branch. The next capture then compares against what is really live, and a later merge has the right base.
2. **A new editor branch starts from the consumer's last applied commit, or from the repo's root commit.** It doesn't start from `desired`. Otherwise, the first capture of live state could silently overwrite intent that is committed but not applied yet. With this rule, the two meet as a conflict instead.
3. **Adapters feeding a `revert` or `block` consumer don't auto-integrate captures.** Without this, a watcher would adopt the drift behind the policy's back. It can be overridden with `confit.adapter.<name>.integrate`.
4. **`block` means the last observed live state differs from what that consumer last applied.** It stays blocked until someone runs `confit integrate` (keep the drift) or `confit apply --force` (discard it).
5. **A dconf directory becomes `<dir>.ini` under the adapter path.** Keys directly in the root go to `_root.ini`.
6. **git runs the merge driver as `confit` from PATH by default.** A Nix store path would break on every upgrade. Use `--driver-command` to override.

## Not built yet

- Session squashing on integration. `confit watch` debouncing already turns a slider drag into one commit.
- A NixOS module (system-wide dconf, other system consumers). The home-manager module covers the user side.
- k8s-style keyed list merges (`name` keys) in the JSON driver.
- Pushing to a remote. The design's open question 1 (one repo per machine or shared) is still open.

## Development

`nix develop` (or direnv) gives a shell with Go, git, gomod2nix, dconf and dbus.

```sh
make test        # go test ./...; dconf tests skip themselves
make test-all    # everything, with a throwaway dconf on a private D-Bus, including the e2e desktop scenario
make build       # nix build .#, which also runs the tests
make check       # nix flake check: gofmt, nixfmt, actionlint
make tidy        # after changing go.mod: go mod tidy and regenerate nix/gomod2nix.toml
```

Outside the dev shell it needs Go 1.26, git ≥ 2.42, and for the dconf tests dconf-cli and dbus.
