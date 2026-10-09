# Git Buffer (`gb`)

Git Buffer records every configuration change a UI makes as a git commit, lets consumers (home-manager, NixOS, scripts) apply those commits later, and records what each consumer applied. Git is the only state. Every command is one-shot, so nothing needs a daemon.

Design: `../design/git-buffer-design.md`. Language choice: `../spike/README.md`.

Status: step 3 of the plan (core library and CLI). Not used on a real desktop yet; that is step 4.

## Quick start

```sh
gb init --bare ~/.local/share/gb/repo.git
export GB_REPO=~/.local/share/gb/repo.git

gb adapter add vscode --type file --file settings.json=$HOME/.config/Code/User/settings.json
gb adapter add dconf  --type dconf --root /org/gnome/
gb consumer add home-manager@laptop --adapter vscode --adapter dconf --drift adopt

gb capture vscode        # snapshot the live file, commit any change, integrate it
gb pending home-manager@laptop
gb status
```

A consumer that applies by itself (home-manager) does:

```sh
target=$(gb prepare home-manager@laptop)   # captures drift first; exit 3 if the policy blocks
home-manager switch --flake ...#$target     # reads the repo at $target
gb applied advance home-manager@laptop "$target"
```

A consumer that gb can apply through its adapters runs `gb apply <consumer>`, which does all three steps.

## Commands

| Command | What it does |
|---|---|
| `gb init [--bare] [--integration NAME] [path]` | Creates the repo, registers the merge drivers in git config, and creates the integration branch with a `.gitattributes`. Run it on every clone, since git config isn't cloned. |
| `gb adapter add` / `gb consumer add` | Write `gitbuffer.adapter.*` / `gitbuffer.consumer.*` git config. |
| `gb capture <adapter>` | Snapshot live state, commit the difference to `edits/<editor>`, integrate. No difference means no commit. |
| `gb edit commit <editor> --dir D [--prefix P]` | For UIs and extensions that hand gb their files directly. |
| `gb integrate [<editor>...]` | Merge editor branches into the integration branch. A conflicting editor is reported and left out; the others still integrate. |
| `gb pending <consumer>` | Commits in `applied/<consumer>..<source>` and the tree diff. |
| `gb prepare <consumer>` | Drift check: capture the consumer's adapters, apply its drift policy, print the commit to apply. |
| `gb apply <consumer> [--force]` | prepare, write the target tree through the adapters, advance. |
| `gb applied advance <consumer> <commit> [--expect OLD]` | Compare-and-swap the applied pointer and append an apply note. Refuses commits outside the source branch. |
| `gb status [--json]` | Unintegrated editors, conflicts, pending work, recorded drift. |
| `gb merge-driver json\|ini %O %A %B` | Invoked by git, never by hand. |

Every command takes `-C <repo>` (or `$GB_REPO`). Most take `--json`. Exit codes: 0 ok, 1 error, 2 usage, 3 conflict or blocked.

## How it maps to the design

- **Refs**: `edits/<editor>`, the integration branch (`desired` by default, configurable, and HEAD is never consulted), `applied/<consumer>`, and `refs/notes/buffer-applied`. All prefixes are git config.
- **Spike rules, all enforced in `internal/gitrepo`**:
  - go-git only reads and writes objects.
  - Every ref move is a `git update-ref --stdin` transaction.
  - Merges are `git merge-tree` with `attr.tree` set to the integration branch.
  - Notes are written by gb with compare-and-swap, never `git notes add`.
- **Merge drivers**: JSON/JSONC and dconf INI. Both try git's line merge first. If that conflicts, they merge key by key. JSONC edits are applied as a patch, so comments and formatting survive. A key changed on both sides leaves normal conflict markers for any mergetool.
- **Commit metadata**: author is the human from git config, committer is `git-buffer (<adapter>)`. Trailers are `Buffer-Editor`, `Buffer-Adapter`, `Buffer-Source`, `Buffer-Session` and `Buffer-Keys`. Summaries look like `vscode: set editor.fontSize = 14`.
- **Drift policies** (`adopt` default, `revert`, `block`) behave as the design describes. The tests cover each one.

## Decisions made while building (worth a look)

1. **An editor branch tracks the live state the adapter last saw.** After an apply, gb adds a sync commit (with the applied commit as a parent) to each affected editor branch. The next capture then compares against what is really live, and a later merge has the right base.
2. **A new editor branch starts from the consumer's last applied commit, or from the repo's root commit.** It doesn't start from `desired`. Otherwise, the first capture of live state could silently overwrite intent that is committed but not applied yet. With this rule, the two meet as a conflict instead.
3. **Adapters feeding a `revert` or `block` consumer don't auto-integrate captures.** Without this, a watcher would adopt the drift behind the policy's back. It can be overridden with `gitbuffer.adapter.<name>.integrate`.
4. **`block` means the last observed live state differs from what that consumer last applied.** It stays blocked until someone runs `gb integrate` (keep the drift) or `gb apply --force` (discard it).
5. **A dconf directory becomes `<dir>.ini` under the adapter path.** Keys directly in the root go to `_root.ini`.
6. **git runs the merge driver as `gb` from PATH by default.** A Nix store path would break on every upgrade. Use `--driver-command` to override.

## Not built yet

- `gb watch` (the tiny debounce-and-exec trigger), commit debouncing and session squashing. Step 4 will need these for dconf.
- systemd `.path`/timer units and Nix packaging, both planned for step 4.
- k8s-style keyed list merges (`name` keys) in the JSON driver.
- Pushing to a remote. The design's open question 1 (one repo per machine or shared) is still open.

## Development

```sh
go test ./...            # dconf tests skip themselves
bash scripts/test.sh     # everything, with a throwaway dconf on a private D-Bus
```

Needs Go 1.26, git ≥ 2.42, and for the dconf tests dconf-cli and dbus.
